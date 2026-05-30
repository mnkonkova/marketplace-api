package projects

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ─── Хелперы для интеграционных тестов ─────────────────────────────

// mkClient создаёт юзера-клиента и возвращает его id. Очистка не нужна —
// тесты пишут уникальные email с timestamp.
func mkClient(t *testing.T, ctx context.Context, repo *Repo, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := strings.ReplaceAll(label, " ", "_") + "-" + time.Now().Format("150405.000000") + "@test.local"
	_, err := repo.db.Exec(ctx, `
INSERT INTO users (id, email, password_hash, kind, role, email_verified_at)
VALUES ($1, $2, '$2a$10$dummynotused', 'client', 'client', now())`,
		id, email)
	if err != nil {
		t.Fatalf("insert client: %v", err)
	}
	return id
}

func mkSpecialist(t *testing.T, ctx context.Context, repo *Repo, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := strings.ReplaceAll(label, " ", "_") + "-" + time.Now().Format("150405.000000") + "@test.local"
	_, err := repo.db.Exec(ctx, `
INSERT INTO users (id, email, password_hash, kind, role, email_verified_at)
VALUES ($1, $2, '$2a$10$dummynotused', 'specialist', 'specialist', now())`,
		id, email)
	if err != nil {
		t.Fatalf("insert specialist: %v", err)
	}
	_, err = repo.db.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name)
VALUES ($1, $2)`,
		id, "Test Spec")
	if err != nil {
		t.Fatalf("insert specialist_profile: %v", err)
	}
	return id
}

func mkLeadWithRecipient(t *testing.T, ctx context.Context, repo *Repo, clientID, specID uuid.UUID, recipientStatus string) uuid.UUID {
	t.Helper()
	leadID := uuid.New()
	_, err := repo.db.Exec(ctx, `
INSERT INTO leads (id, client_user_id, client_name, client_contact, brief)
VALUES ($1, $2, 'Test Client', 'test@example.com', 'тестовый бриф')`,
		leadID, clientID)
	if err != nil {
		t.Fatalf("insert lead: %v", err)
	}
	_, err = repo.db.Exec(ctx, `
INSERT INTO lead_recipients (lead_id, specialist_user_id, status)
VALUES ($1, $2, $3)`,
		leadID, specID, recipientStatus)
	if err != nil {
		t.Fatalf("insert lead_recipient: %v", err)
	}
	return leadID
}

// ─── Тесты ─────────────────────────────────────────────────────────

// TestStartProjectManual: создание manual-проекта должно:
//  1. вставить projects-строку с source=manual, status=active;
//  2. создать 6 стадий и 17 шагов в project_stages/_steps;
//  3. оставить событие в outbox с event_type=project.created.
func TestStartProjectManual(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "manual-client")
	actor := mkClient(t, ctx, repo, "actor")

	in := CreateManualInput{
		ClientUserID:    client,
		TemplateCode:    "video_production",
		TemplateVersion: 1,
		Title:           "Тест-проект manual",
		Source:          SourceManual,
	}
	created, err := svc.StartProjectManual(ctx, in, actor)
	if err != nil {
		t.Fatalf("StartProjectManual: %v", err)
	}
	if created.Source != SourceManual {
		t.Errorf("source = %s, want manual", created.Source)
	}
	if created.Status != StatusActive {
		t.Errorf("status = %s, want active", created.Status)
	}
	if created.StartedAt == nil {
		t.Error("started_at must be set")
	}

	// Stages: ровно 6 в нужном порядке.
	stages, err := repo.ListStages(ctx, created.ID)
	if err != nil {
		t.Fatalf("ListStages: %v", err)
	}
	if len(stages) != 6 {
		t.Errorf("stages = %d, want 6", len(stages))
	}

	// Steps: 17 всего (без фильтра).
	steps, err := repo.ListStepsWithStage(ctx, created.ID, VisibilityNoFilter)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 17 {
		t.Errorf("steps = %d, want 17", len(steps))
	}

	// Outbox: должно быть событие project.created с aggregate_id = project.id.
	var count int
	err = pool.QueryRow(ctx, `
SELECT COUNT(*) FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = 'project.created'`,
		created.ID.String()).Scan(&count)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if count != 1 {
		t.Errorf("outbox events = %d, want 1", count)
	}
}

// TestStartFromRecipient: для маркетплейс-проекта recipient.status=accepted
// обязателен; source автоматически marketplace; повторный старт →
// ErrAlreadyStarted (partial unique).
func TestStartFromRecipient(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "mp-client")
	spec := mkSpecialist(t, ctx, repo, "mp-spec")
	actor := mkClient(t, ctx, repo, "mp-actor")

	t.Run("recipient не accepted → ErrRecipientNotReady", func(t *testing.T) {
		leadID := mkLeadWithRecipient(t, ctx, repo, client, spec, "viewed")
		_, err := svc.StartFromRecipient(ctx, CreateFromRecipientInput{
			LeadID:           leadID,
			SpecialistUserID: spec,
			TemplateCode:     "video_production",
			TemplateVersion:  1,
			Title:            "Test mp",
		}, actor)
		if err == nil || err.Error() != ErrRecipientNotReady.Error() {
			t.Errorf("expected ErrRecipientNotReady, got %v", err)
		}
	})

	t.Run("accepted → ok, source=marketplace", func(t *testing.T) {
		// Новый специалист для чистоты — иначе partial unique с предыдущего теста
		spec2 := mkSpecialist(t, ctx, repo, "mp-spec-ok")
		leadID := mkLeadWithRecipient(t, ctx, repo, client, spec2, "accepted")
		created, err := svc.StartFromRecipient(ctx, CreateFromRecipientInput{
			LeadID:           leadID,
			SpecialistUserID: spec2,
			TemplateCode:     "video_production",
			TemplateVersion:  1,
			Title:            "Test mp",
		}, actor)
		if err != nil {
			t.Fatalf("StartFromRecipient: %v", err)
		}
		if created.Source != SourceMarketplace {
			t.Errorf("source = %s, want marketplace", created.Source)
		}
		if created.LeadID == nil || *created.LeadID != leadID {
			t.Errorf("lead_id mismatch: %v", created.LeadID)
		}
		if created.SpecialistUserID == nil || *created.SpecialistUserID != spec2 {
			t.Errorf("specialist mismatch")
		}

		// Повтор → ErrAlreadyStarted.
		_, err = svc.StartFromRecipient(ctx, CreateFromRecipientInput{
			LeadID:           leadID,
			SpecialistUserID: spec2,
			TemplateCode:     "video_production",
			TemplateVersion:  1,
			Title:            "Test mp again",
		}, actor)
		if err == nil || err.Error() != ErrAlreadyStarted.Error() {
			t.Errorf("expected ErrAlreadyStarted, got %v", err)
		}
	})
}

// TestGetClientFunnel: чужой проект → ErrNotFound; шаги фильтруются
// по visible_to_client (для video_production scenarios: payment OK,
// social_setup и internal_approval скрыты от клиента → не должны
// возвращаться в funnel).
func TestGetClientFunnel(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "funnel-client")
	other := mkClient(t, ctx, repo, "funnel-other")

	created, err := svc.StartProjectManual(ctx, CreateManualInput{
		ClientUserID:    client,
		TemplateCode:    "video_production",
		TemplateVersion: 1,
		Title:           "Funnel test",
		Source:          SourceManual,
	}, client)
	if err != nil {
		t.Fatalf("StartProjectManual: %v", err)
	}

	// Чужой клиент → 404
	if _, err := svc.GetClientFunnel(ctx, created.ID, other); err == nil || err.Error() != ErrNotFound.Error() {
		t.Errorf("expected ErrNotFound for foreign user, got %v", err)
	}

	// Свой клиент → funnel
	f, err := svc.GetClientFunnel(ctx, created.ID, client)
	if err != nil {
		t.Fatalf("GetClientFunnel: %v", err)
	}

	// Подсчёт шагов: для video_production видимы клиенту 15 из 17 (скрыты
	// social_setup и internal_approval).
	totalVisible := 0
	for _, st := range f.Stages {
		for _, s := range st.Steps {
			totalVisible++
			if s.Code == "social_setup" || s.Code == "internal_approval" {
				t.Errorf("step %s should be hidden from client", s.Code)
			}
		}
	}
	if totalVisible != 15 {
		t.Errorf("visible steps = %d, want 15", totalVisible)
	}

	// Прогресс на свежем проекте = 0%; current_step должен быть payment.
	if f.Progress.Percent != 0 {
		t.Errorf("progress = %d, want 0", f.Progress.Percent)
	}
	if f.Progress.CurrentStepID == nil {
		t.Error("current_step должен быть payment")
	}
}
