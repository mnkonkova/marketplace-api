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
