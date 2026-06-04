package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/pipelines"
	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// createSecondPipeline — создаёт ещё одну валидную воронку (3 шага в одной
// стадии) с уникальными названиями шагов, чтобы их можно было отличить от
// исходной из setupPipelineAndProject.
func createSecondPipeline(t *testing.T, pool *pgxpool.Pool, name string) (uuid.UUID, *pipelines.Repo) {
	t.Helper()
	repo := newPipelinesRepo(t, pool)
	pl, err := repo.CreatePipeline(context.Background(), pipelinesCreate(name))
	if err != nil {
		t.Fatalf("create pipeline: %v", err)
	}
	st, err := repo.CreateStage(context.Background(), pl.ID, pipelinesStage("Новая стадия", 0))
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	for i, n := range []string{"Newstep-A", "Newstep-B", "Newstep-C"} {
		if _, err := repo.CreateStep(context.Background(), st.ID, pipelinesStep(n, "team", i)); err != nil {
			t.Fatalf("create step %d: %v", i, err)
		}
	}
	return pl.ID, repo
}

// TestChangeFunnelHappyPath — счастливый путь: проект пересоздаёт стадии
// и шаги из новой воронки, revisions_used обнуляется, started_at обновляется,
// status остаётся active.
func TestChangeFunnelHappyPath(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	newPID, _ := createSecondPipeline(t, pool, "ChangeFunnel target #1")
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, newPID)
	}()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	updated, err := repo.ChangeFunnel(ctx, pid, newPID, uuid.New())
	if err != nil {
		t.Fatalf("ChangeFunnel: %v", err)
	}
	if updated.PipelineID != newPID {
		t.Errorf("pipeline_id: want %s, got %s", newPID, updated.PipelineID)
	}
	if updated.RevisionsUsed != 0 {
		t.Errorf("revisions_used: want 0, got %d", updated.RevisionsUsed)
	}
	if string(updated.Status) != "active" {
		t.Errorf("status: want active, got %s", updated.Status)
	}
	if updated.CompletedAt != nil {
		t.Errorf("completed_at: want nil after change_funnel, got %v", *updated.CompletedAt)
	}

	// Стадии/шаги пересозданы из новой воронки.
	stages, err := repo.LoadStages(ctx, pid)
	if err != nil {
		t.Fatalf("load stages: %v", err)
	}
	if len(stages) != 1 || stages[0].Name != "Новая стадия" {
		t.Fatalf("want 1 stage 'Новая стадия', got %d: %+v", len(stages), stages)
	}

	steps, err := repo.LoadSteps(ctx, pid, false)
	if err != nil {
		t.Fatalf("load steps: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(steps))
	}
	stepNames := map[string]bool{}
	for _, s := range steps {
		stepNames[s.Name] = true
	}
	for _, n := range []string{"Newstep-A", "Newstep-B", "Newstep-C"} {
		if !stepNames[n] {
			t.Errorf("missing step %s in new project_steps", n)
		}
	}

	// Первый шаг должен стартовать в in_progress (тот же контракт что у Create).
	var firstStepStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM project_steps WHERE project_id = $1 ORDER BY sort_order LIMIT 1`,
		pid).Scan(&firstStepStatus); err != nil {
		t.Fatalf("query first step: %v", err)
	}
	if firstStepStatus != "in_progress" {
		t.Errorf("first step status: want in_progress, got %s", firstStepStatus)
	}
}

// TestChangeFunnelDeletesOldSteps — старые project_steps/project_stages
// удаляются (CASCADE). После смены не должно остаться следов исходной
// воронки в project_*.
func TestChangeFunnelDeletesOldSteps(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	newPID, _ := createSecondPipeline(t, pool, "ChangeFunnel target #2")
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, newPID)
	}()

	ctx := context.Background()
	repo := projects.NewRepo(pool)

	// До смены: 2 стадии × 2 шага = 4 шага.
	if _, err := repo.ChangeFunnel(ctx, pid, newPID, uuid.New()); err != nil {
		t.Fatalf("ChangeFunnel: %v", err)
	}

	// Проверяем что в БД нет шагов со старыми названиями.
	var oldCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM project_steps WHERE project_id = $1 AND name IN ('Сделать монтаж','Апрув клиента','Финализация','Оставить отзыв')`,
		pid).Scan(&oldCount); err != nil {
		t.Fatalf("count old steps: %v", err)
	}
	if oldCount != 0 {
		t.Errorf("want 0 old-named steps, got %d", oldCount)
	}
}

// TestChangeFunnelRejectsCancelled — для отменённого проекта ChangeFunnel
// возвращает ErrInvalidTransition (нельзя переоткрывать через смену воронки).
func TestChangeFunnelRejectsCancelled(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	newPID, _ := createSecondPipeline(t, pool, "ChangeFunnel target #3")
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, newPID)
	}()

	ctx := context.Background()
	// Переводим вручную status='cancelled'.
	if _, err := pool.Exec(ctx, `UPDATE projects SET status = 'cancelled' WHERE id = $1`, pid); err != nil {
		t.Fatalf("set cancelled: %v", err)
	}

	repo := projects.NewRepo(pool)
	_, err := repo.ChangeFunnel(ctx, pid, newPID, uuid.New())
	if !errors.Is(err, projects.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition for cancelled project, got %v", err)
	}
}

// TestChangeFunnelRejectsUnknownPipeline — несуществующий target → ErrNotFound.
func TestChangeFunnelRejectsUnknownPipeline(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	_, err := repo.ChangeFunnel(context.Background(), pid, uuid.New(), uuid.New())
	if !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown pipeline, got %v", err)
	}
}

// TestChangeFunnelRejectsInactivePipeline — неактивная воронка тоже даёт
// ErrNotFound (бэк фильтрует WHERE is_active=TRUE при загрузке).
func TestChangeFunnelRejectsInactivePipeline(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	newPID, _ := createSecondPipeline(t, pool, "ChangeFunnel inactive")
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, newPID)
	}()
	// Деактивируем целевую воронку.
	if _, err := pool.Exec(context.Background(),
		`UPDATE pipelines SET is_active = FALSE WHERE id = $1`, newPID); err != nil {
		t.Fatalf("deactivate pipeline: %v", err)
	}

	repo := projects.NewRepo(pool)
	_, err := repo.ChangeFunnel(context.Background(), pid, newPID, uuid.New())
	if !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("want ErrNotFound for inactive pipeline, got %v", err)
	}
}

// TestChangeFunnelRejectsEmptyPipeline — воронка без стадий/шагов даёт
// ErrPipelineEmpty (выловили на ручном тестировании; теперь это 400 на API).
func TestChangeFunnelRejectsEmptyPipeline(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	// Делаем пустую воронку без стадий.
	pipeRepo := newPipelinesRepo(t, pool)
	emptyPL, err := pipeRepo.CreatePipeline(context.Background(), pipelinesCreate("ChangeFunnel empty"))
	if err != nil {
		t.Fatalf("create empty pipeline: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, emptyPL.ID)
	}()

	repo := projects.NewRepo(pool)
	_, err = repo.ChangeFunnel(context.Background(), pid, emptyPL.ID, uuid.New())
	if !errors.Is(err, projects.ErrPipelineEmpty) {
		t.Fatalf("want ErrPipelineEmpty, got %v", err)
	}
}

// TestChangeFunnelEmitsOutboxEvent — после смены в outbox должно появиться
// событие project.funnel_changed с правильным payload.
func TestChangeFunnelEmitsOutboxEvent(t *testing.T) {
	pool := integration.Pool(t)
	_, oldPID, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	newPID, _ := createSecondPipeline(t, pool, "ChangeFunnel outbox")
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, newPID)
	}()

	repo := projects.NewRepo(pool)
	if _, err := repo.ChangeFunnel(context.Background(), pid, newPID, uuid.New()); err != nil {
		t.Fatalf("ChangeFunnel: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM outbox
WHERE aggregate = 'project'
  AND aggregate_id = $1
  AND event_type = 'project.funnel_changed'
  AND payload::text LIKE '%' || $2 || '%'
  AND payload::text LIKE '%' || $3 || '%'`,
		pid.String(), oldPID.String(), newPID.String()).Scan(&count); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if count == 0 {
		t.Errorf("project.funnel_changed event not found in outbox (or missing pipeline ids in payload)")
	}
}
