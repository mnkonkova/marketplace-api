package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/admin"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: полный цикл magic-link: Generate → Consume → email_verified ----

func TestMagicLinkFullCycle(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	// Создаём админа-creator и target-юзера.
	var creatorID, userID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_admin, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, TRUE, now()) RETURNING id`,
		"creator-"+uuid.NewString()+"@x").Scan(&creatorID)
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved)
VALUES ($1, 'x', 'client', TRUE) RETURNING id`,
		"target-"+uuid.NewString()+"@x").Scan(&userID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{creatorID, userID})

	repo := admin.NewRepo(pool)

	// Generate
	raw, expiresAt, err := repo.GenerateInvite(ctx, userID, creatorID, 24*time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(raw) < 32 {
		t.Errorf("raw token too short: %d", len(raw))
	}
	if expiresAt.Before(time.Now().Add(23*time.Hour)) {
		t.Errorf("expires_at должен быть через ~24 часа")
	}

	// Consume — должен вернуть user_id
	got, err := repo.ConsumeInvite(ctx, raw)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got != userID {
		t.Errorf("consume returned %v, want %v", got, userID)
	}

	// После consume — email_verified_at должен стать NOT NULL
	var verified bool
	_ = pool.QueryRow(ctx,
		`SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`,
		userID).Scan(&verified)
	if !verified {
		t.Errorf("email_verified_at не выставлен после ConsumeInvite")
	}
}

// ---- ТЕСТ: повторный Consume того же токена — ErrInviteInvalid ----

func TestMagicLinkConsumeTwice(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	var creatorID, userID uuid.UUID
	_ = pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, kind, is_admin, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, TRUE, now()) RETURNING id`, "c-"+uuid.NewString()+"@x").Scan(&creatorID)
	_ = pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, kind, is_approved)
VALUES ($1, 'x', 'client', TRUE) RETURNING id`, "u-"+uuid.NewString()+"@x").Scan(&userID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{creatorID, userID})

	repo := admin.NewRepo(pool)
	raw, _, err := repo.GenerateInvite(ctx, userID, creatorID, 24*time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := repo.ConsumeInvite(ctx, raw); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	// Вторая попытка — токен помечен used_at
	_, err = repo.ConsumeInvite(ctx, raw)
	if !errors.Is(err, admin.ErrInviteInvalid) {
		t.Errorf("second consume: want ErrInviteInvalid, got %v", err)
	}
}

// ---- ТЕСТ: GenerateInvite гасит предыдущие invites ----

func TestMagicLinkInvalidatesPrevious(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	var creatorID, userID uuid.UUID
	_ = pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, kind, is_admin, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, TRUE, now()) RETURNING id`, "c-"+uuid.NewString()+"@x").Scan(&creatorID)
	_ = pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, kind, is_approved)
VALUES ($1, 'x', 'client', TRUE) RETURNING id`, "u-"+uuid.NewString()+"@x").Scan(&userID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{creatorID, userID})

	repo := admin.NewRepo(pool)
	raw1, _, _ := repo.GenerateInvite(ctx, userID, creatorID, 24*time.Hour)
	raw2, _, err := repo.GenerateInvite(ctx, userID, creatorID, 24*time.Hour)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}

	// Первый токен теперь invalid (used_at проставлен).
	_, err = repo.ConsumeInvite(ctx, raw1)
	if !errors.Is(err, admin.ErrInviteInvalid) {
		t.Errorf("первый токен после нового GenerateInvite должен быть invalid")
	}
	// Второй — рабочий.
	if _, err := repo.ConsumeInvite(ctx, raw2); err != nil {
		t.Errorf("второй токен: %v", err)
	}
}

// ---- ТЕСТ: SetApproved для menager ----

func TestApproveRevokeManager(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	var mgrID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_manager, is_approved)
VALUES ($1, 'x', 'specialist', TRUE, FALSE) RETURNING id`,
		"mgr-"+uuid.NewString()+"@x").Scan(&mgrID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, mgrID)

	repo := admin.NewRepo(pool)

	// Approve
	if err := repo.SetApproved(ctx, mgrID, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var approved bool
	_ = pool.QueryRow(ctx, `SELECT is_approved FROM users WHERE id=$1`, mgrID).Scan(&approved)
	if !approved {
		t.Errorf("not approved after SetApproved(true)")
	}

	// Revoke
	if err := repo.SetApproved(ctx, mgrID, false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT is_approved FROM users WHERE id=$1`, mgrID).Scan(&approved)
	if approved {
		t.Errorf("approved after SetApproved(false)")
	}
}

// ---- ТЕСТ: SetApproved на не-manager → ErrNotManager ----

func TestSetApprovedRejectsNonManager(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	var clientID uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved)
VALUES ($1, 'x', 'client', TRUE) RETURNING id`,
		"cl-"+uuid.NewString()+"@x").Scan(&clientID)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, clientID)

	repo := admin.NewRepo(pool)
	err := repo.SetApproved(ctx, clientID, true)
	if !errors.Is(err, admin.ErrNotManager) {
		t.Errorf("want ErrNotManager, got %v", err)
	}
}
