package projects

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationDB — открывает локальную dev-БД по DATABASE_URL.
// Если переменной нет, тест помечается skipped (не падает на CI без БД).
func integrationDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	return pool
}

// TestLoadActiveTemplate_VideoProductionV1 — на seed-данных из миграции
// 00011 должны быть ровно 6 стадий и 17 шагов, в правильном порядке,
// с правильными visible-флагами для каждого ключевого шага.
func TestLoadActiveTemplate_VideoProductionV1(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snap, err := repo.LoadActiveTemplate(ctx, "video_production", 1)
	if err != nil {
		t.Fatalf("LoadActiveTemplate: %v", err)
	}

	if snap.Code != "video_production" || snap.Version != 1 {
		t.Errorf("template identity mismatch: %s v%d", snap.Code, snap.Version)
	}
	if snap.RevisionsIncluded != 2 {
		t.Errorf("revisions_included = %d, want 2", snap.RevisionsIncluded)
	}

	if got := len(snap.Stages); got != 6 {
		t.Fatalf("stages count = %d, want 6", got)
	}
	wantStageCodes := []string{"start", "prep", "shooting", "editing", "acceptance", "delivery"}
	for i, want := range wantStageCodes {
		if snap.Stages[i].Code != want {
			t.Errorf("stage[%d].code = %s, want %s", i, snap.Stages[i].Code, want)
		}
	}

	// Точечная проверка визибилити-флагов по таблице брифа §3.
	type expected struct {
		stage     string
		step      string
		owner     StepOwner
		visClient bool
		visSpec   bool
		duration  int
		weight    int
	}
	checks := []expected{
		{"start", "payment", OwnerClient, true, false, 1, 1},
		{"start", "escrow_hold", OwnerSystem, true, false, 0, 1},
		{"prep", "social_setup", OwnerTeam, false, false, 2, 2},        // обе скрыты
		{"editing", "internal_approval", OwnerTeam, false, true, 1, 1}, // скрыт клиенту, виден специалисту
		{"acceptance", "client_review", OwnerClient, true, true, 2, 2},
		{"delivery", "nps", OwnerClient, true, false, 1, 1},
		{"delivery", "review", OwnerClient, true, false, 1, 1}, // скрыт специалисту (не видит отзыв о себе)
	}
	for _, c := range checks {
		var stage *SnapshotStage
		for i := range snap.Stages {
			if snap.Stages[i].Code == c.stage {
				stage = &snap.Stages[i]
				break
			}
		}
		if stage == nil {
			t.Errorf("stage %s not found", c.stage)
			continue
		}
		var step *SnapshotStep
		for i := range stage.Steps {
			if stage.Steps[i].Code == c.step {
				step = &stage.Steps[i]
				break
			}
		}
		if step == nil {
			t.Errorf("step %s/%s not found", c.stage, c.step)
			continue
		}
		if step.Owner != c.owner {
			t.Errorf("%s/%s: owner=%s, want %s", c.stage, c.step, step.Owner, c.owner)
		}
		if step.VisibleToClient != c.visClient {
			t.Errorf("%s/%s: visible_to_client=%v, want %v", c.stage, c.step, step.VisibleToClient, c.visClient)
		}
		if step.VisibleToSpecialist != c.visSpec {
			t.Errorf("%s/%s: visible_to_specialist=%v, want %v", c.stage, c.step, step.VisibleToSpecialist, c.visSpec)
		}
		if step.DurationDays != c.duration {
			t.Errorf("%s/%s: duration=%d, want %d", c.stage, c.step, step.DurationDays, c.duration)
		}
		if step.Weight != c.weight {
			t.Errorf("%s/%s: weight=%d, want %d", c.stage, c.step, step.Weight, c.weight)
		}
	}

	// Итого 17 шагов.
	total := 0
	for _, s := range snap.Stages {
		total += len(s.Steps)
	}
	if total != 17 {
		t.Errorf("total steps = %d, want 17", total)
	}
}

func TestLoadActiveTemplate_NotFound(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := repo.LoadActiveTemplate(ctx, "nonexistent", 1)
	if err == nil {
		t.Fatal("expected ErrTemplateNotFound, got nil")
	}
	if err.Error() != ErrTemplateNotFound.Error() {
		t.Errorf("got %v, want %v", err, ErrTemplateNotFound)
	}
}
