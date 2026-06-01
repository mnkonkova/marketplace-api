package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: AutoSkipReviewStep переводит review-шаг в skipped ----

func TestAutoSkipReviewStep(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	ctx := context.Background()

	// Делаем последний client-шаг review-шагом и переводим в waiting_client
	// с просроченным deadline (час назад). Через CTE — UPDATE+ORDER BY+LIMIT
	// в Postgres нельзя напрямую.
	var reviewStepID uuid.UUID
	if err := pool.QueryRow(ctx, `
WITH target AS (
    SELECT id FROM project_steps
    WHERE project_id = $1 AND owner = 'client'
    ORDER BY sort_order DESC LIMIT 1
)
UPDATE project_steps SET is_review = TRUE, status = 'waiting_client',
                          review_deadline = now() - interval '1 hour',
                          started_at = now() - interval '8 days'
WHERE id IN (SELECT id FROM target)
RETURNING id`, pid).Scan(&reviewStepID); err != nil {
		t.Fatalf("setup review step: %v", err)
	}
	_ = clientID

	repo := projects.NewRepo(pool)
	if err := repo.AutoSkipReviewStep(ctx, pid, reviewStepID); err != nil {
		t.Fatalf("auto-skip: %v", err)
	}

	// Шаг должен стать skipped.
	var status string
	_ = pool.QueryRow(ctx,
		`SELECT status FROM project_steps WHERE id = $1`, reviewStepID).Scan(&status)
	if status != "skipped" {
		t.Errorf("status after auto-skip: %s, want skipped", status)
	}

	// Событие должно появиться (actor_type=system, from=waiting_client, to=skipped).
	var actorType, fromS, toS string
	if err := pool.QueryRow(ctx, `
SELECT actor_type, from_status, to_status
FROM project_step_events
WHERE project_id = $1 AND step_id = $2 AND event_kind = 'step_transition'
ORDER BY created_at DESC LIMIT 1`, pid, reviewStepID).Scan(&actorType, &fromS, &toS); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if actorType != "system" || fromS != "waiting_client" || toS != "skipped" {
		t.Errorf("event: type=%s, from=%s, to=%s", actorType, fromS, toS)
	}
}

// ---- ТЕСТ: AutoSkipReviewStep идемпотентен (вторая попытка не падает) ----

func TestAutoSkipReviewStepIdempotent(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	ctx := context.Background()

	var reviewStepID uuid.UUID
	_ = pool.QueryRow(ctx, `
WITH target AS (
    SELECT id FROM project_steps
    WHERE project_id = $1 AND owner = 'client'
    ORDER BY sort_order DESC LIMIT 1
)
UPDATE project_steps SET is_review = TRUE, status = 'waiting_client',
                          review_deadline = now() - interval '1 hour'
WHERE id IN (SELECT id FROM target)
RETURNING id`, pid).Scan(&reviewStepID)

	repo := projects.NewRepo(pool)
	if err := repo.AutoSkipReviewStep(ctx, pid, reviewStepID); err != nil {
		t.Fatalf("first auto-skip: %v", err)
	}
	// Вторая попытка — шаг уже не waiting_client → ErrInvalidTransition.
	err := repo.AutoSkipReviewStep(ctx, pid, reviewStepID)
	if !errors.Is(err, projects.ErrInvalidTransition) {
		t.Errorf("second auto-skip: want ErrInvalidTransition, got %v", err)
	}
}

// ---- ТЕСТ: ListExpiredReviewSteps находит только просроченные ----

func TestListExpiredReviewSteps(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	ctx := context.Background()

	// Один просроченный, один со свежим дедлайном.
	var expired, future uuid.UUID
	_ = pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' ORDER BY sort_order ASC LIMIT 1)
UPDATE project_steps SET is_review=TRUE, status='waiting_client',
       review_deadline=now() - interval '1 hour'
WHERE id IN (SELECT id FROM t) RETURNING id`, pid).Scan(&expired)
	_ = pool.QueryRow(ctx, `
WITH t AS (SELECT id FROM project_steps WHERE project_id=$1 AND owner='client' ORDER BY sort_order DESC LIMIT 1)
UPDATE project_steps SET is_review=TRUE, status='waiting_client',
       review_deadline=now() + interval '5 days'
WHERE id IN (SELECT id FROM t) RETURNING id`, pid).Scan(&future)

	repo := projects.NewRepo(pool)
	items, err := repo.ListExpiredReviewSteps(ctx, time.Now())
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	// items может содержать и других проекта; нас интересуют только наши.
	found := false
	for _, x := range items {
		if x.StepID == expired {
			found = true
		}
		if x.StepID == future {
			t.Errorf("future-deadline step попал в expired-list")
		}
	}
	if !found {
		t.Errorf("expired step not found in list")
	}
}
