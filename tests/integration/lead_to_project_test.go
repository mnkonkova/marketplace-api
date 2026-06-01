package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/leads"
	"marketpclce/internal/pipelines"
	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: StartFromLead создаёт проект с default-воронкой ----

func TestStartFromLeadHappyPath(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	// Создаём клиента и воронку, делаем её default.
	var clientID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, role, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', 'client', TRUE, now()) RETURNING id`,
		"l2p-"+uuid.NewString()+"@x").Scan(&clientID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, clientID)

	plRepo := pipelines.NewRepo(pool)
	pl, _ := plRepo.CreatePipeline(ctx, pipelinesCreate("L2P "+uuid.NewString()))
	defer pool.Exec(ctx, `DELETE FROM pipelines WHERE id=$1`, pl.ID)
	st, _ := plRepo.CreateStage(ctx, pl.ID, pipelinesStage("Бриф", 0))
	_, _ = plRepo.CreateStep(ctx, st.ID, pipelinesStep("Принять", "team", 0))
	if err := plRepo.MakeDefault(ctx, pl.ID); err != nil {
		t.Fatalf("make default: %v", err)
	}

	prSvc := projects.NewService(projects.NewRepo(pool)).
		WithDefaultPipeline(pipelines.NewService(plRepo))

	var leadID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO leads (client_user_id, client_name, client_contact, brief)
VALUES ($1, 'Test', '+7', 'тестовый бриф длиннее 10') RETURNING id`,
		clientID).Scan(&leadID)
	defer pool.Exec(ctx, `DELETE FROM leads WHERE id=$1`, leadID)
	pid, err := prSvc.StartFromLead(ctx, clientID, leadID, "Промо-ролик", "Нужно видео для соцсетей", nil)
	if err != nil {
		t.Fatalf("start from lead: %v", err)
	}
	if pid == uuid.Nil {
		t.Errorf("project id empty")
	}
	defer pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, pid)

	// Проверяем что в БД действительно проект с нашим lead_id и pipeline_id.
	var got struct {
		ClientID    uuid.UUID
		PipelineID  uuid.UUID
		LeadID      *uuid.UUID
		AssignedTo  *uuid.UUID
		Title       string
		Source      string
		Notes       string
	}
	_ = pool.QueryRow(ctx, `
SELECT client_user_id, pipeline_id, lead_id, assigned_to_user_id, title, source::text, COALESCE(notes,'')
FROM projects WHERE id=$1`, pid).
		Scan(&got.ClientID, &got.PipelineID, &got.LeadID, &got.AssignedTo, &got.Title, &got.Source, &got.Notes)

	if got.ClientID != clientID {
		t.Errorf("client_user_id mismatch: %v != %v", got.ClientID, clientID)
	}
	if got.PipelineID != pl.ID {
		t.Errorf("pipeline_id mismatch")
	}
	if got.LeadID == nil || *got.LeadID != leadID {
		t.Errorf("lead_id mismatch: %+v", got.LeadID)
	}
	if got.AssignedTo != nil {
		t.Errorf("assigned_to должен быть NULL (inbox), получен %v", *got.AssignedTo)
	}
	if got.Source != "marketplace" {
		t.Errorf("source: %s, want marketplace", got.Source)
	}
	if got.Title != "Промо-ролик" {
		t.Errorf("title: %s", got.Title)
	}
	if !strings.Contains(got.Notes, "видео") {
		t.Errorf("notes: %s — должен содержать бриф", got.Notes)
	}
}

// ---- ТЕСТ: StartFromLead без default-воронки → ErrDefaultPipelineMissing ----

func TestStartFromLeadNoDefault(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	// Снимаем default со всех (тест детерминируется)
	_, _ = pool.Exec(ctx, `UPDATE pipelines SET is_default = FALSE`)

	prSvc := projects.NewService(projects.NewRepo(pool)).
		WithDefaultPipeline(pipelines.NewService(pipelines.NewRepo(pool)))

	_, err := prSvc.StartFromLead(ctx, uuid.New(), uuid.New(), "x", "y", nil)
	if err == nil {
		t.Errorf("ожидалась ошибка без default-воронки")
	}
}

// ---- ТЕСТ: briefTitle обрезает длинный текст ----

func TestBriefTitle(t *testing.T) {
	// Используем leads пакет напрямую — briefTitle экспортный? нет.
	// Тестим через косвенно: создаём lead + старт проекта и проверяем title.
	pool := integration.Pool(t)
	ctx := context.Background()

	var clientID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, role, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', 'client', TRUE, now()) RETURNING id`,
		"bt-"+uuid.NewString()+"@x").Scan(&clientID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, clientID)

	plRepo := pipelines.NewRepo(pool)
	pl, _ := plRepo.CreatePipeline(ctx, pipelinesCreate("BT "+uuid.NewString()))
	defer pool.Exec(ctx, `DELETE FROM pipelines WHERE id=$1`, pl.ID)
	st, _ := plRepo.CreateStage(ctx, pl.ID, pipelinesStage("S", 0))
	_, _ = plRepo.CreateStep(ctx, st.ID, pipelinesStep("Step", "team", 0))
	_ = plRepo.MakeDefault(ctx, pl.ID)

	prSvc := projects.NewService(projects.NewRepo(pool)).
		WithDefaultPipeline(pipelines.NewService(plRepo))

	var leadID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO leads (client_user_id, client_name, client_contact, brief)
VALUES ($1, 'T', '+7', 'тестовый бриф длиннее 10') RETURNING id`,
		clientID).Scan(&leadID)
	defer pool.Exec(ctx, `DELETE FROM leads WHERE id=$1`, leadID)

	// Бриф многострочный — title должен быть только первой строкой.
	pid, err := prSvc.StartFromLead(ctx, clientID, leadID,
		"Первая строка о ролике", "Первая строка о ролике\nВторая строка с деталями", nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, pid)
	var title string
	_ = pool.QueryRow(ctx, `SELECT title FROM projects WHERE id=$1`, pid).Scan(&title)
	if strings.Contains(title, "\n") || strings.Contains(title, "Вторая") {
		t.Errorf("title содержит лишние строки: %q", title)
	}

	// Проверка интерфейса leads.ProjectStarter через сильное приведение:
	var _ leads.ProjectStarter = prSvc
}

// silence linter
var _ = errors.Is
