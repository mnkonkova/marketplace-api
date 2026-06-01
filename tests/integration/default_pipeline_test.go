package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/pipelines"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: MakeDefault эксклюзивный — снимает default с других ----

func TestMakeDefaultExclusive(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	repo := pipelines.NewRepo(pool)

	pl1, err := repo.CreatePipeline(ctx, pipelinesCreate("Воронка A "+uuid.NewString()))
	if err != nil {
		t.Fatalf("create pl1: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM pipelines WHERE id=$1`, pl1.ID)
	pl2, err := repo.CreatePipeline(ctx, pipelinesCreate("Воронка B "+uuid.NewString()))
	if err != nil {
		t.Fatalf("create pl2: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM pipelines WHERE id=$1`, pl2.ID)

	if err := repo.MakeDefault(ctx, pl1.ID); err != nil {
		t.Fatalf("make default pl1: %v", err)
	}

	defID, err := repo.GetDefaultPipelineID(ctx)
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if defID != pl1.ID {
		t.Errorf("default id = %v, want %v", defID, pl1.ID)
	}

	// Делаем pl2 дефолтной — pl1 должна потерять флаг.
	if err := repo.MakeDefault(ctx, pl2.ID); err != nil {
		t.Fatalf("make default pl2: %v", err)
	}
	defID2, _ := repo.GetDefaultPipelineID(ctx)
	if defID2 != pl2.ID {
		t.Errorf("after switch: default id = %v, want %v", defID2, pl2.ID)
	}

	// Проверка БД: ровно один is_default=TRUE.
	var cnt int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM pipelines WHERE is_default = TRUE`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("default count: %d, want 1", cnt)
	}
}

// ---- ТЕСТ: GetDefaultPipelineID без default → ErrNotFound ----

func TestGetDefaultPipelineNotFound(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	// Снимаем default со всех чтобы тест был детерминистичен.
	_, _ = pool.Exec(ctx, `UPDATE pipelines SET is_default = FALSE`)

	repo := pipelines.NewRepo(pool)
	_, err := repo.GetDefaultPipelineID(ctx)
	if !errors.Is(err, pipelines.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
