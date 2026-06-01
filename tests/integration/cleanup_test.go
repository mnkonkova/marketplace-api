package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: CleanupOldCompletedProjects удаляет done с completed_at < cutoff ----

func TestCleanupOldCompletedProjects(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pidOld, cleanupOld := setupPipelineAndProject(t, pool)
	defer cleanupOld()
	_, _, pidFresh, cleanupFresh := setupPipelineAndProject(t, pool)
	defer cleanupFresh()
	_, _, pidActive, cleanupActive := setupPipelineAndProject(t, pool)
	defer cleanupActive()

	ctx := context.Background()
	// pidOld: done + completed_at старше cutoff (8 дней назад)
	_, _ = pool.Exec(ctx,
		`UPDATE projects SET status='done', completed_at=now()-interval '8 days' WHERE id=$1`,
		pidOld)
	// pidFresh: done но completed_at свежее (1 день назад)
	_, _ = pool.Exec(ctx,
		`UPDATE projects SET status='done', completed_at=now()-interval '1 day' WHERE id=$1`,
		pidFresh)
	// pidActive: active (не done) — не трогать даже если старше
	_, _ = pool.Exec(ctx,
		`UPDATE projects SET status='active', updated_at=now()-interval '10 days' WHERE id=$1`,
		pidActive)

	repo := projects.NewRepo(pool)
	n, err := repo.CleanupOldCompletedProjects(ctx, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Удалён должен быть как минимум pidOld. Может быть больше (другие
	// старые done в БД от прошлых тестов — допустимо).
	if n < 1 {
		t.Errorf("want >=1 deleted, got %d", n)
	}

	// pidOld исчез
	var existsOld bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, pidOld).Scan(&existsOld)
	if existsOld {
		t.Errorf("pidOld должен быть удалён")
	}
	// pidFresh жив (свежее cutoff'а)
	var existsFresh bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, pidFresh).Scan(&existsFresh)
	if !existsFresh {
		t.Errorf("pidFresh должен быть жив")
	}
	// pidActive жив (не done)
	var existsActive bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, pidActive).Scan(&existsActive)
	if !existsActive {
		t.Errorf("pidActive (active) должен быть жив")
	}

	_ = uuid.Nil // sil linter
}

// ---- ТЕСТ: cancelled удаляется только если старше cancelledRetention ----

func TestCleanupCancelledRespectsRetention(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pidOld, cleanupOld := setupPipelineAndProject(t, pool)
	defer cleanupOld()
	_, _, pidRecent, cleanupRecent := setupPipelineAndProject(t, pool)
	defer cleanupRecent()

	ctx := context.Background()
	// pidOld: cancelled, updated_at 40 дней назад — должен удалиться
	_, _ = pool.Exec(ctx,
		`UPDATE projects SET status='cancelled', updated_at=now()-interval '40 days' WHERE id=$1`,
		pidOld)
	// pidRecent: cancelled, updated_at 10 дней назад — жив (порог 30)
	_, _ = pool.Exec(ctx,
		`UPDATE projects SET status='cancelled', updated_at=now()-interval '10 days' WHERE id=$1`,
		pidRecent)

	repo := projects.NewRepo(pool)
	if _, err := repo.CleanupOldCompletedProjects(ctx, 0, 30*24*time.Hour); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var aliveOld, aliveRecent bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, pidOld).Scan(&aliveOld)
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, pidRecent).Scan(&aliveRecent)
	if aliveOld {
		t.Errorf("cancelled 40d должен быть удалён")
	}
	if !aliveRecent {
		t.Errorf("cancelled 10d должен быть жив")
	}
}

// ---- ТЕСТ: retention=0 → ничего не удаляет ----

func TestCleanupNoOp(t *testing.T) {
	pool := integration.Pool(t)
	repo := projects.NewRepo(pool)
	n, err := repo.CleanupOldCompletedProjects(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("cleanup with retention=0: %v", err)
	}
	if n != 0 {
		t.Errorf("retention=0 должно быть no-op, удалено %d", n)
	}
}
