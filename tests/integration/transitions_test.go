package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: Approve переводит client waiting_client → done ----

func TestClientApproveStep(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	ctx := context.Background()

	// Делаем client-шаг в waiting_client.
	var stepID uuid.UUID
	_ = pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' ORDER BY sort_order ASC LIMIT 1)
UPDATE project_steps SET status='waiting_client' WHERE id IN (SELECT id FROM t) RETURNING id`,
		pid).Scan(&stepID)

	svc := projects.NewService(projects.NewRepo(pool))
	step, err := svc.Approve(ctx, pid, stepID, clientID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if step.Status != projects.StepStatusDone {
		t.Errorf("status after approve: %s, want done", step.Status)
	}
}

// ---- ТЕСТ: RequestRevision переводит шаг в rejected + bump revisions_used ----

func TestRequestRevisionBumpsRevisions(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	ctx := context.Background()

	// Готовим: предыдущий team-шаг в done, текущий client-шаг в waiting_client.
	_, _ = pool.Exec(ctx, `
UPDATE project_steps SET status='done'
WHERE project_id=$1 AND owner='team'`, pid)
	var stepID uuid.UUID
	_ = pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' ORDER BY sort_order ASC LIMIT 1)
UPDATE project_steps SET status='waiting_client' WHERE id IN (SELECT id FROM t) RETURNING id`,
		pid).Scan(&stepID)

	svc := projects.NewService(projects.NewRepo(pool))
	if _, err := svc.RequestRevision(ctx, pid, stepID, clientID, "переделать"); err != nil {
		t.Fatalf("request_revision: %v", err)
	}

	var revUsed int
	_ = pool.QueryRow(ctx, `SELECT revisions_used FROM projects WHERE id=$1`, pid).Scan(&revUsed)
	if revUsed != 1 {
		t.Errorf("revisions_used: %d, want 1", revUsed)
	}
}

// ---- ТЕСТ: исчерпание правок → ErrRevisionsExhausted + status=dispute ----

func TestRevisionsExhaustedDispute(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	ctx := context.Background()

	// Зеро правок (лимит 2, используем 2 заранее → следующая = 3 > limit).
	_, _ = pool.Exec(ctx,
		`UPDATE projects SET revisions_used = revisions_included WHERE id = $1`, pid)
	_, _ = pool.Exec(ctx,
		`UPDATE project_steps SET status='done' WHERE project_id=$1 AND owner='team'`, pid)

	var stepID uuid.UUID
	_ = pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' ORDER BY sort_order ASC LIMIT 1)
UPDATE project_steps SET status='waiting_client' WHERE id IN (SELECT id FROM t) RETURNING id`,
		pid).Scan(&stepID)

	svc := projects.NewService(projects.NewRepo(pool))
	_, err := svc.RequestRevision(ctx, pid, stepID, clientID, "лимит")
	if !errors.Is(err, projects.ErrRevisionsExhausted) {
		t.Errorf("want ErrRevisionsExhausted, got %v", err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM projects WHERE id=$1`, pid).Scan(&status)
	if status != "dispute" {
		t.Errorf("project status: %s, want dispute", status)
	}
}

// ---- ТЕСТ: SubmitReview только для is_review шага ----

func TestSubmitReviewRequiresReviewFlag(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	ctx := context.Background()

	// Берём НЕ review-шаг в waiting_client.
	var stepID uuid.UUID
	_ = pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' AND is_review=FALSE ORDER BY sort_order ASC LIMIT 1)
UPDATE project_steps SET status='waiting_client' WHERE id IN (SELECT id FROM t) RETURNING id`,
		pid).Scan(&stepID)

	svc := projects.NewService(projects.NewRepo(pool))
	_, err := svc.SubmitReview(ctx, pid, stepID, clientID)
	if !errors.Is(err, projects.ErrNotReviewStep) {
		t.Errorf("want ErrNotReviewStep, got %v", err)
	}
}

// ---- ТЕСТ: Approve ловит non-client шаг ----

func TestApproveRejectsNonClientStep(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	ctx := context.Background()

	// Team-шаг в in_progress.
	var stepID uuid.UUID
	_ = pool.QueryRow(ctx, `
SELECT id FROM project_steps WHERE project_id=$1 AND owner='team' LIMIT 1`,
		pid).Scan(&stepID)

	svc := projects.NewService(projects.NewRepo(pool))
	_, err := svc.Approve(ctx, pid, stepID, clientID)
	if !errors.Is(err, projects.ErrNotClientStep) {
		t.Errorf("want ErrNotClientStep, got %v", err)
	}
}

// Правки должны вернуть предыдущий шаг команды в работу.
//
// Клиент нажимает «правки», его шаг уходит в rejected — и на этом всё бы
// закончилось, если бы работа не возвращалась исполнителю. Ответ 200
// приходит в любом случае: возврат сделан «best-effort», и его неудача
// не считается ошибкой. Снаружи это выглядит как «отправлено на
// доработку», хотя ни один шаг не в работе и над проектом никто не
// работает — узнать об этом неоткуда.
func TestRequestRevisionReturnsPreviousTeamStep(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	ctx := context.Background()

	// Команда сдала работу, клиент смотрит.
	if _, err := pool.Exec(ctx,
		`UPDATE project_steps SET status='done' WHERE project_id=$1 AND owner='team'`, pid); err != nil {
		t.Fatalf("подготовка шагов: %v", err)
	}
	var clientStep uuid.UUID
	if err := pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' ORDER BY sort_order LIMIT 1)
UPDATE project_steps SET status='waiting_client' WHERE id IN (SELECT id FROM t) RETURNING id`,
		pid).Scan(&clientStep); err != nil {
		t.Fatalf("подготовка клиентского шага: %v", err)
	}

	svc := projects.NewService(projects.NewRepo(pool))
	if _, err := svc.RequestRevision(ctx, pid, clientStep, clientID, "переснять первый кадр"); err != nil {
		t.Fatalf("request revision: %v", err)
	}

	// Клиентский шаг отклонён — это видно.
	var clientStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM project_steps WHERE id = $1`, clientStep).Scan(&clientStatus); err != nil {
		t.Fatalf("статус клиентского шага: %v", err)
	}
	if clientStatus != "rejected" {
		t.Errorf("шаг клиента после правок: %s, ожидали rejected", clientStatus)
	}

	// А работа обязана вернуться команде: без этого «на доработке» —
	// пустые слова.
	var inProgress int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM project_steps
WHERE project_id = $1 AND owner = 'team' AND status = 'in_progress'`, pid).Scan(&inProgress); err != nil {
		t.Fatalf("подсчёт шагов в работе: %v", err)
	}
	if inProgress == 0 {
		t.Error("после запроса правок ни один шаг команды не в работе — над проектом никто не работает")
	}
}
