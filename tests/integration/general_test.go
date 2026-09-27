package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Общий проект: клиент выбирает исполнителя и ставит один срок, исполнитель
// сдаёт, клиент принимает. Ни воронки, ни менеджера — поэтому и setup здесь
// свой, а не общий setupPipelineAndProject.

// setupGeneralActors — клиент и опубликованный специалист.
func setupGeneralActors(t *testing.T, pool *pgxpool.Pool) (clientID, specID uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, now()) RETURNING id`,
		"gen-client-"+uuid.NewString()+"@example.com").Scan(&clientID); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"gen-spec-"+uuid.NewString()+"@example.com").Scan(&specID); err != nil {
		t.Fatalf("create specialist: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, is_published)
VALUES ($1, 'Исполнитель Тестовый', TRUE)`, specID); err != nil {
		t.Fatalf("create profile: %v", err)
	}

	cleanup = func() {
		// outbox без FK на projects — чистим сами, иначе воркер разошлёт
		// тестовые события в n8n.
		_, _ = pool.Exec(ctx, `
DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id IN (
  SELECT id::text FROM projects WHERE client_user_id = $1)`, clientID)
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE client_user_id = $1`, clientID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`,
			[]uuid.UUID{clientID, specID})
	}
	return clientID, specID, cleanup
}

func generalInput(clientID, specID uuid.UUID) projects.CreateGeneralInput {
	return projects.CreateGeneralInput{
		ClientID:     clientID,
		SpecialistID: specID,
		Title:        "Смонтировать ролик",
		Brief:        "Три минуты, вертикаль",
		DueDate:      time.Now().UTC().AddDate(0, 0, 7),
	}
}

// ---- ТЕСТ: проект заводится без воронки ----

// Воронки у общего проекта нет вовсе: pipeline_id остаётся пустым, шаги не
// материализуются. До 00034 колонка была NOT NULL, и такой проект просто
// не вставлялся.
func TestCreateGeneralHasNoPipeline(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, err := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Status != "active" {
		t.Errorf("status: %s", p.Status)
	}
	if p.SpecialistID != specID {
		t.Errorf("исполнитель не тот: %s", p.SpecialistID)
	}
	if p.SpecialistName != "Исполнитель Тестовый" {
		t.Errorf("имя исполнителя: %q", p.SpecialistName)
	}

	var pipelineID *uuid.UUID
	var kind string
	var steps int
	if err := pool.QueryRow(ctx,
		`SELECT p.pipeline_id, p.kind::text,
		        (SELECT COUNT(*) FROM project_steps WHERE project_id = p.id)
		 FROM projects p WHERE p.id = $1`, p.ID).Scan(&pipelineID, &kind, &steps); err != nil {
		t.Fatalf("query project: %v", err)
	}
	if pipelineID != nil {
		t.Errorf("у общего проекта не должно быть воронки, есть %s", pipelineID)
	}
	if kind != "general" {
		t.Errorf("kind: %s", kind)
	}
	if steps != 0 {
		t.Errorf("шаги не должны материализоваться, их %d", steps)
	}
}

// «Уходит исполнителю в бот» — это событие в outbox. Менеджера в проекте
// нет, и других способов узнать о задаче у исполнителя тоже нет.
func TestCreateGeneralEmitsEventForPerformer(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, err := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `
SELECT payload FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = 'project.general_created'`,
		p.ID.String()).Scan(&payload); err != nil {
		t.Fatalf("нет события о новом проекте: %v", err)
	}
	body := string(payload)
	if !strings.Contains(body, specID.String()) || !strings.Contains(body, "general") {
		t.Errorf("в событии нет исполнителя или вида проекта: %s", body)
	}
}

// ---- ТЕСТ: кого нельзя выбрать исполнителем ----

func TestCreateGeneralRejectsBadSpecialist(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	t.Run("непубликованный профиль", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE specialist_profiles SET is_published = FALSE WHERE user_id = $1`, specID); err != nil {
			t.Fatalf("unpublish: %v", err)
		}
		defer pool.Exec(ctx,
			`UPDATE specialist_profiles SET is_published = TRUE WHERE user_id = $1`, specID)

		if _, err := svc.CreateGeneral(ctx, generalInput(clientID, specID)); !errors.Is(err, projects.ErrSpecialistUnavailable) {
			t.Errorf("want ErrSpecialistUnavailable, got %v", err)
		}
	})

	t.Run("заказ у самого себя", func(t *testing.T) {
		in := generalInput(clientID, clientID)
		if _, err := svc.CreateGeneral(ctx, in); !errors.Is(err, projects.ErrInvalidInput) {
			t.Errorf("want ErrInvalidInput, got %v", err)
		}
	})

	t.Run("срок в прошлом", func(t *testing.T) {
		in := generalInput(clientID, specID)
		in.DueDate = time.Now().UTC().AddDate(0, 0, -1)
		if _, err := svc.CreateGeneral(ctx, in); !errors.Is(err, projects.ErrInvalidInput) {
			t.Errorf("want ErrInvalidInput, got %v", err)
		}
	})

	t.Run("срок дальше года", func(t *testing.T) {
		in := generalInput(clientID, specID)
		in.DueDate = time.Now().UTC().AddDate(2, 0, 0)
		if _, err := svc.CreateGeneral(ctx, in); !errors.Is(err, projects.ErrInvalidInput) {
			t.Errorf("want ErrInvalidInput, got %v", err)
		}
	})
}

// ---- ТЕСТ: сдача и приёмка ----

func TestDeliverThenAcceptClosesProject(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, err := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d, err := svc.Deliver(ctx, projects.DeliverInput{
		ProjectID: p.ID, UserID: specID, Note: "Готово",
		Materials: []projects.MaterialInput{
			{Kind: "video", Title: "Ролик", URL: "https://disk.example.com/final.mp4"},
		},
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if d.Attempt != 1 || d.Decision != projects.DeliveryPending {
		t.Errorf("первая сдача: attempt=%d decision=%s", d.Attempt, d.Decision)
	}
	if len(d.Materials) != 1 {
		t.Fatalf("материал не приложился: %d", len(d.Materials))
	}

	closed, err := svc.AcceptDelivery(ctx, p.ID, clientID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if closed.Status != "done" {
		t.Errorf("после приёмки проект должен быть done, он %s", closed.Status)
	}
	if closed.CompletedAt == nil {
		t.Errorf("не проставлен completed_at")
	}
	if len(closed.Deliveries) != 1 || closed.Deliveries[0].Decision != projects.DeliveryAccepted {
		t.Errorf("журнал сдач: %+v", closed.Deliveries)
	}
	// Материал виден в карточке — клиент забирает работу именно оттуда.
	if len(closed.Deliveries[0].Materials) != 1 {
		t.Errorf("материалы пропали из карточки: %+v", closed.Deliveries[0])
	}
}

// Двойной клик на «сдать» не должен ни падать пятисоткой, ни заводить
// вторую незакрытую сдачу: тогда непонятно, какую из них принимает клиент.
func TestSecondDeliveryBlockedWhilePending(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	first := projects.DeliverInput{ProjectID: p.ID, UserID: specID, Note: "раз"}
	if _, err := svc.Deliver(ctx, first); err != nil {
		t.Fatalf("first deliver: %v", err)
	}
	second := projects.DeliverInput{ProjectID: p.ID, UserID: specID, Note: "два"}
	if _, err := svc.Deliver(ctx, second); !errors.Is(err, projects.ErrDeliveryPending) {
		t.Errorf("want ErrDeliveryPending, got %v", err)
	}
}

// ---- ТЕСТ: доработки ----

func TestReworkCountsAndRunsOut(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if p.RevisionsIncluded < 1 {
		t.Fatalf("в проекте нет ни одной правки по умолчанию: %d", p.RevisionsIncluded)
	}

	for i := 1; i <= p.RevisionsIncluded; i++ {
		if _, err := svc.Deliver(ctx, projects.DeliverInput{
			ProjectID: p.ID, UserID: specID, Note: "попытка",
		}); err != nil {
			t.Fatalf("deliver %d: %v", i, err)
		}
		got, err := svc.ReworkDelivery(ctx, p.ID, clientID, "не тот хронометраж")
		if err != nil {
			t.Fatalf("rework %d: %v", i, err)
		}
		if got.RevisionsUsed != i {
			t.Errorf("после %d-й правки revisions_used=%d", i, got.RevisionsUsed)
		}
		if got.Status != "active" {
			t.Errorf("возврат на доработку не должен закрывать проект: %s", got.Status)
		}
	}

	// Правки кончились: следующая сдача сдаётся, но вернуть её уже нельзя.
	if _, err := svc.Deliver(ctx, projects.DeliverInput{
		ProjectID: p.ID, UserID: specID, Note: "последняя",
	}); err != nil {
		t.Fatalf("final deliver: %v", err)
	}
	if _, err := svc.ReworkDelivery(ctx, p.ID, clientID, "опять не то"); !errors.Is(err, projects.ErrReworkLimit) {
		t.Errorf("want ErrReworkLimit, got %v", err)
	}
	// А принять — можно.
	if _, err := svc.AcceptDelivery(ctx, p.ID, clientID); err != nil {
		t.Errorf("accept после исчерпания правок: %v", err)
	}
}

func TestReworkRequiresReason(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	_, _ = svc.Deliver(ctx, projects.DeliverInput{ProjectID: p.ID, UserID: specID, Note: "готово"})
	if _, err := svc.ReworkDelivery(ctx, p.ID, clientID, "   "); !errors.Is(err, projects.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput, got %v", err)
	}
}

func TestAcceptWithoutDelivery(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if _, err := svc.AcceptDelivery(ctx, p.ID, clientID); !errors.Is(err, projects.ErrNoPendingDelivery) {
		t.Errorf("want ErrNoPendingDelivery, got %v", err)
	}
}

// ---- ТЕСТ: отмена ----

// Отмена проекта со сданной работой — способ не платить за уже сделанное.
func TestCancelBlockedWhileDeliveryPending(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	_, _ = svc.Deliver(ctx, projects.DeliverInput{ProjectID: p.ID, UserID: specID, Note: "готово"})

	if err := svc.CancelGeneral(ctx, p.ID, clientID, "передумал"); !errors.Is(err, projects.ErrDeliveryPending) {
		t.Errorf("want ErrDeliveryPending, got %v", err)
	}

	// Вернули на доработку — теперь отменить можно.
	if _, err := svc.ReworkDelivery(ctx, p.ID, clientID, "не то"); err != nil {
		t.Fatalf("rework: %v", err)
	}
	if err := svc.CancelGeneral(ctx, p.ID, clientID, "передумал"); err != nil {
		t.Errorf("cancel: %v", err)
	}
	got, err := svc.GetGeneral(ctx, p.ID, clientID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("status: %s", got.Status)
	}
}

// В отменённый проект нельзя ни сдать, ни принять.
func TestClosedProjectRejectsEverything(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err := svc.CancelGeneral(ctx, p.ID, clientID, ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := svc.Deliver(ctx, projects.DeliverInput{
		ProjectID: p.ID, UserID: specID, Note: "всё равно сдам",
	}); !errors.Is(err, projects.ErrProjectClosed) {
		t.Errorf("deliver в отменённый: want ErrProjectClosed, got %v", err)
	}
	if _, err := svc.AcceptDelivery(ctx, p.ID, clientID); !errors.Is(err, projects.ErrProjectClosed) {
		t.Errorf("accept в отменённом: want ErrProjectClosed, got %v", err)
	}
}

// ---- ТЕСТ: чужой проект ----

// «Не мой проект» и «нет такого» отвечают одинаково: иначе перебором id
// можно узнать, какие проекты существуют.
func TestGeneralHiddenFromStranger(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()
	strangerID, _, cleanup2 := setupGeneralActors(t, pool)
	defer cleanup2()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))

	if _, err := svc.GetGeneral(ctx, p.ID, strangerID); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("посторонний видит карточку: %v", err)
	}
	if _, err := svc.Deliver(ctx, projects.DeliverInput{
		ProjectID: p.ID, UserID: strangerID, Note: "сдаю чужое",
	}); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("посторонний сдал работу: %v", err)
	}
	if _, err := svc.AcceptDelivery(ctx, p.ID, strangerID); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("посторонний принял работу: %v", err)
	}
	// Список исполнителя чужой проект не показывает.
	items, err := svc.ListGeneral(ctx, strangerID, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, it := range items {
		if it.ID == p.ID {
			t.Errorf("чужой проект в списке исполнителя")
		}
	}
}

// ---- ТЕСТ: материалы ----

func TestDeliveryRejectsBadMaterials(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()
	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))

	cases := []struct {
		name string
		mat  projects.MaterialInput
	}{
		{"javascript-ссылка", projects.MaterialInput{Kind: "link", Title: "клик", URL: "javascript:alert(1)"}},
		{"неизвестный вид", projects.MaterialInput{Kind: "archive", Title: "архив", URL: "https://e.com/a.zip"}},
		{"без названия", projects.MaterialInput{Kind: "link", Title: "", URL: "https://e.com/a"}},
		{"не ссылка", projects.MaterialInput{Kind: "link", Title: "что-то", URL: "просто текст"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Deliver(ctx, projects.DeliverInput{
				ProjectID: p.ID, UserID: specID, Materials: []projects.MaterialInput{tc.mat},
			})
			if !errors.Is(err, projects.ErrInvalidInput) {
				t.Errorf("want ErrInvalidInput, got %v", err)
			}
		})
	}

	// Пустая сдача — тоже не сдача.
	if _, err := svc.Deliver(ctx, projects.DeliverInput{ProjectID: p.ID, UserID: specID}); !errors.Is(err, projects.ErrInvalidInput) {
		t.Errorf("пустая сдача прошла: %v", err)
	}
}

// ---- ТЕСТ: просрочка ----

func TestOverdueFlag(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, _ := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if p.Overdue {
		t.Errorf("свежий проект со сроком через неделю не просрочен")
	}
	// Сдвигаем срок в прошлое мимо сервиса: валидация такого не пропустит,
	// а проверяем мы поведение уже существующего проекта.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET due_date = current_date - 1 WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("shift due_date: %v", err)
	}
	got, err := svc.GetGeneral(ctx, p.ID, clientID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Overdue {
		t.Errorf("вчерашний срок — просрочка")
	}

	// Отменённый проект просрочкой не считается: его никто не ждёт.
	if err := svc.CancelGeneral(ctx, p.ID, clientID, ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ = svc.GetGeneral(ctx, p.ID, clientID)
	if got.Overdue {
		t.Errorf("отменённый проект помечен просроченным")
	}
}

// Общий проект живёт без воронки. Клиентский список обязан его показать,
// а не упасть на NULL в pipeline_id.
func TestClientListIncludesGeneralProject(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()
	p, err := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	items, err := svc.ListClientProjects(ctx, clientID)
	if err != nil {
		t.Fatalf("список проектов клиента упал: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("общий проект не попал в список клиента")
	}
}
