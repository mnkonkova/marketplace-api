package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// makeManager — заводит юзера-менеджера (kind=manager, is_approved=TRUE).
// Cleanup ложится на caller'а.
func makeManager(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	email := "mgr-" + uuid.NewString() + "@example.com"
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
INSERT INTO users (email, password_hash, kind, is_approved, is_manager, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, TRUE, now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	return id
}

// ---- Claim ----

// TestClaimUnassigned — менеджер берёт проект без ответственного.
// После Claim assigned_to_user_id = managerID; Repo.GetByIDForManager его видит.
func TestClaimUnassigned(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	repo := projects.NewRepo(pool)
	if err := repo.Claim(context.Background(), pid, mgr); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	p, err := repo.GetByIDForManager(context.Background(), pid, mgr)
	if err != nil {
		t.Fatalf("GetByIDForManager: %v", err)
	}
	if p.AssignedToUserID == nil || *p.AssignedToUserID != mgr {
		t.Fatalf("assigned_to_user_id: want %s, got %v", mgr, p.AssignedToUserID)
	}
}

// TestClaimIdempotent — повторный Claim тем же менеджером не возвращает ошибку.
func TestClaimIdempotent(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	repo := projects.NewRepo(pool)
	if err := repo.Claim(context.Background(), pid, mgr); err != nil {
		t.Fatalf("Claim #1: %v", err)
	}
	if err := repo.Claim(context.Background(), pid, mgr); err != nil {
		t.Fatalf("Claim #2 (idempotent): %v", err)
	}
}

// TestClaimAlreadyTaken — попытка взять проект, который уже взят другим
// менеджером → ErrAlreadyClaimed (фронт показывает «уже взят»).
func TestClaimAlreadyTaken(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr1 := makeManager(t, pool)
	mgr2 := makeManager(t, pool)
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{mgr1, mgr2})
	}()

	repo := projects.NewRepo(pool)
	if err := repo.Claim(context.Background(), pid, mgr1); err != nil {
		t.Fatalf("Claim by mgr1: %v", err)
	}
	err := repo.Claim(context.Background(), pid, mgr2)
	if !errors.Is(err, projects.ErrAlreadyClaimed) {
		t.Fatalf("want ErrAlreadyClaimed, got %v", err)
	}
}

// TestClaimUnknownProject — несуществующий проект → ErrNotFound.
func TestClaimUnknownProject(t *testing.T) {
	pool := integration.Pool(t)
	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	repo := projects.NewRepo(pool)
	err := repo.Claim(context.Background(), uuid.New(), mgr)
	if !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---- GetByIDForManager: новая логика unassigned-доступа ----

// TestGetByIDForManagerOwn — менеджер видит свой проект.
func TestGetByIDForManagerOwn(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	repo := projects.NewRepo(pool)
	if err := repo.Claim(context.Background(), pid, mgr); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	p, err := repo.GetByIDForManager(context.Background(), pid, mgr)
	if err != nil {
		t.Fatalf("GetByIDForManager: %v", err)
	}
	if p.ID != pid {
		t.Errorf("project id mismatch")
	}
}

// TestGetByIDForManagerUnassigned — НОВАЯ логика после фикса:
// менеджер видит unassigned-проект, чтобы взять его в работу из деталей.
func TestGetByIDForManagerUnassigned(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	repo := projects.NewRepo(pool)
	// Никто не делал Claim, assigned_to_user_id остаётся NULL.
	p, err := repo.GetByIDForManager(context.Background(), pid, mgr)
	if err != nil {
		t.Fatalf("GetByIDForManager unassigned: %v", err)
	}
	if p.ID != pid {
		t.Errorf("project id mismatch")
	}
	if p.AssignedToUserID != nil {
		t.Errorf("want assigned_to=NULL, got %v", *p.AssignedToUserID)
	}
}

// TestGetByIDForManagerForeign — менеджер НЕ видит проект, назначенный
// другому менеджеру. Возвращается ErrNotFound (не выдаём факт существования).
func TestGetByIDForManagerForeign(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr1 := makeManager(t, pool)
	mgr2 := makeManager(t, pool)
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{mgr1, mgr2})
	}()

	repo := projects.NewRepo(pool)
	if err := repo.Claim(context.Background(), pid, mgr1); err != nil {
		t.Fatalf("Claim by mgr1: %v", err)
	}
	_, err := repo.GetByIDForManager(context.Background(), pid, mgr2)
	if !errors.Is(err, projects.ErrNotFound) {
		t.Fatalf("want ErrNotFound for foreign project, got %v", err)
	}
}

// TestGetByIDForManagerAdminBypass — managerID=uuid.Nil даёт админский
// доступ: любой проект отдаётся.
func TestGetByIDForManagerAdminBypass(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	repo := projects.NewRepo(pool)
	if err := repo.Claim(context.Background(), pid, mgr); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// admin запрос: managerID=uuid.Nil
	p, err := repo.GetByIDForManager(context.Background(), pid, uuid.Nil)
	if err != nil {
		t.Fatalf("admin bypass: %v", err)
	}
	if p.ID != pid {
		t.Errorf("project id mismatch")
	}
}
