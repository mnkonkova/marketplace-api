package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: ApproveProposedSpecialist копирует lead_recipient → specialist ----

func TestApproveProposedSpecialist(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	spec := makeSpecialist(t, pool, true)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, spec)

	// Имитируем StartFromLead: ставим proposed.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET lead_recipient_specialist_id=$2 WHERE id=$1`,
		pid, spec); err != nil {
		t.Fatalf("set proposed: %v", err)
	}

	repo := projects.NewRepo(pool)
	got, err := repo.ApproveProposedSpecialist(ctx, pid, clientID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got != spec {
		t.Errorf("вернулось %s, ожидался %s", got, spec)
	}

	var specCol *uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT specialist_user_id FROM projects WHERE id=$1`, pid).Scan(&specCol)
	if specCol == nil || *specCol != spec {
		t.Errorf("specialist_user_id не записался: %v", specCol)
	}

	// Событие в ленте.
	var events int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM project_step_events WHERE project_id=$1 AND event_kind='specialist_approved'`,
		pid).Scan(&events)
	if events != 1 {
		t.Errorf("ожидалось 1 событие specialist_approved, %d", events)
	}
}

// ---- ТЕСТ: approve без proposed → ErrNoProposedSpecialist ----

func TestApproveProposedSpecialistEmpty(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	if _, err := repo.ApproveProposedSpecialist(ctx, pid, uuid.New()); !errors.Is(err, projects.ErrNoProposedSpecialist) {
		t.Errorf("ожидалась ErrNoProposedSpecialist, %v", err)
	}
}

// ---- ТЕСТ: reject снимает proposed без трогания specialist_user_id ----

func TestRejectProposedSpecialist(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	spec := makeSpecialist(t, pool, true)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, spec)

	_, _ = pool.Exec(ctx,
		`UPDATE projects SET lead_recipient_specialist_id=$2 WHERE id=$1`, pid, spec)

	repo := projects.NewRepo(pool)
	if err := repo.RejectProposedSpecialist(ctx, pid, clientID, "не профильно"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var proposed, confirmed *uuid.UUID
	_ = pool.QueryRow(ctx,
		`SELECT lead_recipient_specialist_id, specialist_user_id FROM projects WHERE id=$1`,
		pid).Scan(&proposed, &confirmed)
	if proposed != nil {
		t.Errorf("proposed не снят: %v", proposed)
	}
	if confirmed != nil {
		t.Errorf("specialist_user_id трогать не должны: %v", confirmed)
	}
}
