package integration_test

import (
	"context"
	"testing"
	"time"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"

	"github.com/google/uuid"
)

// loadStepsByName — мапим step.name → step (для устойчивых ассертов).
func loadStepsByName(t *testing.T, repo *projects.Repo, pid uuid.UUID) map[string]projects.Step {
	t.Helper()
	steps, err := repo.LoadSteps(context.Background(), pid, false)
	if err != nil {
		t.Fatalf("load steps: %v", err)
	}
	m := map[string]projects.Step{}
	for _, s := range steps {
		m[s.Name] = s
	}
	return m
}

// ---- ТЕСТ: MoveProjectToStep вперёд через клиентский шаг → клиентский → skipped ----

func TestMoveStepSkipsClientStepForward(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	byName := loadStepsByName(t, repo, pid)
	finalizationID := byName["Финализация"].ID
	clientApprovalID := byName["Апрув клиента"].ID
	editID := byName["Сделать монтаж"].ID

	// Двигаем проект на "Финализация" (team-шаг stage2). Между текущим и
	// целевым стоит client-шаг "Апрув клиента" в pending — раньше это давало
	// ErrStageBlocked, теперь client-шаг должен пометиться skipped.
	if _, err := repo.MoveProjectToStep(ctx, pid, finalizationID, uuid.New(), 7*24*time.Hour, nil); err != nil {
		t.Fatalf("move step: %v", err)
	}

	byName = loadStepsByName(t, repo, pid)
	if got := byName["Сделать монтаж"].Status; got != projects.StepStatusDone {
		t.Errorf("монтаж: %s, want done", got)
	}
	if got := byName["Апрув клиента"].Status; got != projects.StepStatusSkipped {
		t.Errorf("апрув клиента: %s, want skipped", got)
	}
	if byName["Апрув клиента"].CompletedAt == nil {
		t.Errorf("skipped step must have completed_at set")
	}
	if got := byName["Финализация"].Status; got != projects.StepStatusInProgress {
		t.Errorf("финализация: %s, want in_progress", got)
	}
	// Финальный client-step не трогаем.
	if got := byName["Оставить отзыв"].Status; got != projects.StepStatusPending {
		t.Errorf("отзыв: %s, want pending", got)
	}

	// Должно быть событие step_skipped_by_staff на пропущенном клиентском шаге.
	var skippedEvents int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM project_step_events
WHERE project_id=$1 AND step_id=$2 AND event_kind='step_skipped_by_staff'`,
		pid, clientApprovalID).Scan(&skippedEvents); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if skippedEvents != 1 {
		t.Errorf("ожидалось 1 событие step_skipped_by_staff, получено %d", skippedEvents)
	}
	_ = editID
}

// ---- ТЕСТ: MoveProjectToStep назад сбрасывает skipped client-шаг в pending ----

func TestMoveStepBackwardResetsSkippedClient(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	byName := loadStepsByName(t, repo, pid)
	finalizationID := byName["Финализация"].ID
	editID := byName["Сделать монтаж"].ID

	// Двигаем вперёд (skipped on client).
	if _, err := repo.MoveProjectToStep(ctx, pid, finalizationID, uuid.New(), 7*24*time.Hour, nil); err != nil {
		t.Fatalf("forward: %v", err)
	}
	// Двигаем назад на "Сделать монтаж".
	if _, err := repo.MoveProjectToStep(ctx, pid, editID, uuid.New(), 7*24*time.Hour, nil); err != nil {
		t.Fatalf("backward: %v", err)
	}

	byName = loadStepsByName(t, repo, pid)
	if got := byName["Апрув клиента"].Status; got != projects.StepStatusPending {
		t.Errorf("после возврата апрув клиента: %s, want pending (сбросилось)", got)
	}
	if byName["Апрув клиента"].CompletedAt != nil {
		t.Errorf("после возврата completed_at должно быть NULL")
	}
}

// ---- ТЕСТ: MoveProjectToStep на review-шаг ставит дедлайн ----

func TestMoveStepActivatesReviewDeadline(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	// помечаем последний шаг (Оставить отзыв) как is_review для теста дедлайна.
	if _, err := pool.Exec(context.Background(),
		`UPDATE project_steps SET is_review=TRUE
		 WHERE project_id=$1 AND name='Оставить отзыв'`, pid); err != nil {
		t.Fatalf("mark review: %v", err)
	}

	repo := projects.NewRepo(pool)
	ctx := context.Background()
	byName := loadStepsByName(t, repo, pid)
	reviewID := byName["Оставить отзыв"].ID

	if _, err := repo.MoveProjectToStep(ctx, pid, reviewID, uuid.New(), 5*24*time.Hour, nil); err != nil {
		t.Fatalf("move: %v", err)
	}

	byName = loadStepsByName(t, repo, pid)
	if got := byName["Оставить отзыв"].Status; got != projects.StepStatusWaitingClient {
		t.Errorf("review-step status: %s, want waiting_client", got)
	}
	if byName["Оставить отзыв"].ReviewDeadline == nil {
		t.Errorf("review_deadline должен быть установлен")
	}
}
