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

// setupPipelineAndProject — создаёт тестового клиента, тестовую воронку
// (2 стадии × 2 шага) и стартует проект. Возвращает id'ы и cleanup-функцию.
// Cleanup делает DELETE FROM projects/pipelines/users в обратном порядке;
// каскадно уходят stages/steps/events.
func setupPipelineAndProject(t *testing.T, pool *pgxpool.Pool) (clientID, pipelineID, projectID uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	// тестовый client (email уникальный с timestamp, чтобы не конфликтовать)
	email := "it-" + uuid.NewString() + "@example.com"
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, now()) RETURNING id`, email).Scan(&clientID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	pipelinesRepo := newPipelinesRepo(t, pool)
	pl, err := pipelinesRepo.CreatePipeline(ctx, pipelinesCreate("IT воронка"))
	if err != nil {
		t.Fatalf("create pipeline: %v", err)
	}
	pipelineID = pl.ID

	st1, err := pipelinesRepo.CreateStage(ctx, pipelineID, pipelinesStage("Стадия 1", 0))
	if err != nil {
		t.Fatalf("create stage1: %v", err)
	}
	st2, err := pipelinesRepo.CreateStage(ctx, pipelineID, pipelinesStage("Стадия 2", 1))
	if err != nil {
		t.Fatalf("create stage2: %v", err)
	}
	// шаги stage1: team, client (чтобы advance заблокировался без апрува клиента)
	_, _ = pipelinesRepo.CreateStep(ctx, st1.ID, pipelinesStep("Сделать монтаж", "team", 0))
	_, _ = pipelinesRepo.CreateStep(ctx, st1.ID, pipelinesStep("Апрув клиента", "client", 1))
	// шаги stage2: team
	_, _ = pipelinesRepo.CreateStep(ctx, st2.ID, pipelinesStep("Финализация", "team", 0))
	_, _ = pipelinesRepo.CreateStep(ctx, st2.ID, pipelinesStep("Оставить отзыв", "client", 1))

	projectsRepo := projects.NewRepo(pool)
	projectID, err = projectsRepo.StartProject(ctx, projects.StartProjectInput{
		ClientUserID: &clientID,
		PipelineID:   pipelineID,
		Title:        "IT-проект",
		Source:       projects.SourceManual,
	})
	if err != nil {
		t.Fatalf("start project: %v", err)
	}

	cleanup = func() {
		// Чистим outbox для этого проекта — иначе worker при следующем
		// запуске отправит project.created (и любые другие test-события)
		// в n8n → Telegram. На outbox нет FK к projects, поэтому сами.
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`,
			projectID.String())
		// каскады: projects → stages, steps, events, comments
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID)
		_, _ = pool.Exec(ctx, `DELETE FROM pipelines WHERE id = $1`, pipelineID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, clientID)
	}
	return clientID, pipelineID, projectID, cleanup
}

// ---- ТЕСТ: StartProject делает снэпшот стадий и шагов ----

func TestStartProjectSnapshot(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	stages, err := repo.LoadStages(ctx, pid)
	if err != nil {
		t.Fatalf("load stages: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages snapshot, got %d", len(stages))
	}
	if stages[0].Name != "Стадия 1" || stages[1].Name != "Стадия 2" {
		t.Errorf("stages: %s, %s", stages[0].Name, stages[1].Name)
	}

	steps, err := repo.LoadSteps(ctx, pid, false)
	if err != nil {
		t.Fatalf("load steps: %v", err)
	}
	if len(steps) != 4 {
		t.Fatalf("want 4 steps snapshot, got %d", len(steps))
	}
	// Первый шаг — in_progress (StartProject активирует первый автоматически)
	if steps[0].Status != projects.StepStatusInProgress {
		t.Errorf("first step status: %s, want in_progress", steps[0].Status)
	}
	if steps[0].StartedAt == nil {
		t.Errorf("first step started_at must be set")
	}
	// Остальные — pending
	for i := 1; i < len(steps); i++ {
		if steps[i].Status != projects.StepStatusPending {
			t.Errorf("step %d status: %s, want pending", i, steps[i].Status)
		}
	}
}

// ---- ТЕСТ: AdvanceStage блокируется client-шагом ----

func TestAdvanceStageBlockedByClient(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	// Изначально в стадии 1 есть незавершённый client-шаг → advance должен 409.
	_, err := repo.AdvanceStage(ctx, pid, clientID, 7*24*time.Hour, nil)
	if !errors.Is(err, projects.ErrStageBlocked) {
		t.Fatalf("want ErrStageBlocked, got %v", err)
	}
}

// ---- ТЕСТ: AdvanceStage проходит когда client-шагов нет ----

func TestAdvanceStageOK(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	// Закрываем client-шаг стадии 1 руками (имитируем что клиент апрувнул).
	if _, err := pool.Exec(ctx,
		`UPDATE project_steps SET status = 'done', completed_at = now()
		 WHERE project_id = $1 AND owner = 'client'`, pid); err != nil {
		t.Fatalf("close client steps: %v", err)
	}

	_, err := repo.AdvanceStage(ctx, pid, clientID, 7*24*time.Hour, nil)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}

	// Стадия 1 должна иметь completed_at; первый шаг стадии 2 — активирован.
	var stage1Completed *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT completed_at FROM project_stages WHERE project_id = $1 AND sort_order = 0`,
		pid).Scan(&stage1Completed); err != nil {
		t.Fatalf("query stage1: %v", err)
	}
	if stage1Completed == nil {
		t.Errorf("stage 1 completed_at must be set after advance")
	}

	var step2Status string
	if err := pool.QueryRow(ctx, `
SELECT ps.status FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
WHERE st.project_id = $1 AND st.sort_order = 1 AND ps.sort_order = 0`,
		pid).Scan(&step2Status); err != nil {
		t.Fatalf("query first step of stage2: %v", err)
	}
	// Первый шаг стадии 2 — owner=team → должен стать in_progress.
	if step2Status != "in_progress" {
		t.Errorf("first step of stage 2: %s, want in_progress", step2Status)
	}
}

// ---- ТЕСТ: optimistic-lock на AdvanceStage ----

func TestAdvanceStageOptimisticLock(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	// Закрываем client-шаги.
	_, _ = pool.Exec(ctx,
		`UPDATE project_steps SET status = 'done', completed_at = now()
		 WHERE project_id = $1 AND owner = 'client'`, pid)

	// Шлём заведомо устаревший updated_at — должен быть ErrConflict.
	stale := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := repo.AdvanceStage(ctx, pid, clientID, 7*24*time.Hour, &stale)
	if !errors.Is(err, projects.ErrConflict) {
		t.Fatalf("want ErrConflict on stale updated_at, got %v", err)
	}
}

// ---- ТЕСТ: MoveProjectToStage вперёд и назад ----

func TestMoveProjectToStageBackAndForward(t *testing.T) {
	pool := integration.Pool(t)
	clientID, plID, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	// id стадии 2 (pipeline_stages — резолв по sort_order работает на бэке)
	var stage2PipelineID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM pipeline_stages WHERE pipeline_id = $1 AND sort_order = 1`,
		plID).Scan(&stage2PipelineID); err != nil {
		t.Fatalf("get pipeline stage2: %v", err)
	}

	// 1. Закрываем client-шаги и движемся вперёд.
	_, _ = pool.Exec(ctx,
		`UPDATE project_steps SET status='done', completed_at=now()
		 WHERE project_id=$1 AND owner='client'`, pid)
	if _, err := repo.MoveProjectToStage(ctx, pid, stage2PipelineID, clientID, 7*24*time.Hour, nil); err != nil {
		t.Fatalf("move forward: %v", err)
	}

	// Проверка: первый шаг стадии 2 теперь активен.
	var s2Status string
	if err := pool.QueryRow(ctx, `
SELECT ps.status FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
WHERE st.project_id=$1 AND st.sort_order=1 AND ps.sort_order=0`,
		pid).Scan(&s2Status); err != nil {
		t.Fatalf("query step: %v", err)
	}
	if s2Status != "in_progress" {
		t.Errorf("forward: first step stage2 = %s, want in_progress", s2Status)
	}

	// 2. Двигаемся назад в стадию 1 — pipeline id первой стадии.
	var stage1PipelineID uuid.UUID
	_ = pool.QueryRow(ctx,
		`SELECT id FROM pipeline_stages WHERE pipeline_id = $1 AND sort_order = 0`,
		plID).Scan(&stage1PipelineID)

	if _, err := repo.MoveProjectToStage(ctx, pid, stage1PipelineID, clientID, 7*24*time.Hour, nil); err != nil {
		t.Fatalf("move backward: %v", err)
	}

	// Стадия 2 должна быть полностью сброшена в pending.
	rows, err := pool.Query(ctx, `
SELECT ps.status FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
WHERE st.project_id = $1 AND st.sort_order = 1`, pid)
	if err != nil {
		t.Fatalf("query stage2 steps: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		_ = rows.Scan(&status)
		if status != "pending" {
			t.Errorf("backward: stage2 step status = %s, want pending", status)
		}
	}
}

// ---- ТЕСТ: Claim атомарность через UPDATE ... WHERE assigned_to IS NULL ----

func TestClaimAlreadyClaimed(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	// Создаём двух менеджеров.
	m1, m2 := uuid.New(), uuid.New()
	for _, mid := range []uuid.UUID{m1, m2} {
		if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, password_hash, kind, is_manager, is_approved)
VALUES ($1, $2, 'x', 'specialist', TRUE, TRUE)`,
			mid, "mgr-"+mid.String()+"@example.com"); err != nil {
			t.Fatalf("create manager: %v", err)
		}
		defer pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, mid)
	}

	// Первый claim — успех.
	if err := repo.Claim(ctx, pid, m1); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// Второй — ErrAlreadyClaimed.
	err := repo.Claim(ctx, pid, m2)
	if !errors.Is(err, projects.ErrAlreadyClaimed) {
		t.Errorf("second claim: want ErrAlreadyClaimed, got %v", err)
	}
	// Тот же менеджер ещё раз — идемпотентно, без ошибки.
	if err := repo.Claim(ctx, pid, m1); err != nil {
		t.Errorf("idempotent re-claim: %v", err)
	}
}

// ---- ТЕСТ: HardDeletePipeline блокируется при наличии проекта ----

func TestHardDeletePipelineBlocked(t *testing.T) {
	pool := integration.Pool(t)
	_, plID, _, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	pr := newPipelinesRepo(t, pool)
	err := pr.HardDeletePipeline(context.Background(), plID)
	if err == nil {
		t.Fatalf("HardDelete on pipeline with project: want error, got nil")
	}
	// Имя ошибки специфичное в pipelines пакете
	if !strings.Contains(err.Error(), "active") && !strings.Contains(err.Error(), "projects") {
		t.Logf("error type: %T — %v (matched generic)", err, err)
	}
}
