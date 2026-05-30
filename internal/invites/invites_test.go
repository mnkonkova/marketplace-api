package invites

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

func mkUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := strings.ReplaceAll(label, " ", "_") + "-" + time.Now().Format("150405.000000") + "@test.local"
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, password_hash, kind, role)
VALUES ($1, $2, '!magic-link-only!', 'client', 'client')`,
		id, email)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// TestRedeemHappyPath: Generate → Redeem → email_verified_at != NULL,
// userID совпадает.
func TestRedeemHappyPath(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := mkUser(t, ctx, pool, "redeem-happy")

	gen, err := repo.Generate(ctx, userID, uuid.Nil, DefaultTTL)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gen.RawToken == "" || !strings.Contains(gen.RawToken, ".") {
		t.Errorf("invalid compound token format: %q", gen.RawToken)
	}

	got, err := repo.Redeem(ctx, gen.RawToken)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got != userID {
		t.Errorf("redeemed userID = %v, want %v", got, userID)
	}

	// users.email_verified_at должен теперь быть NOT NULL.
	var verifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT email_verified_at FROM users WHERE id = $1`, userID).Scan(&verifiedAt); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if verifiedAt == nil {
		t.Error("email_verified_at должен быть NOT NULL после redeem")
	}
}

// TestRedeemAlreadyUsed: повторный redeem того же токена → ErrUsed.
func TestRedeemAlreadyUsed(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := mkUser(t, ctx, pool, "redeem-used")
	gen, _ := repo.Generate(ctx, userID, uuid.Nil, DefaultTTL)

	if _, err := repo.Redeem(ctx, gen.RawToken); err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	_, err := repo.Redeem(ctx, gen.RawToken)
	if err == nil || err.Error() != ErrUsed.Error() {
		t.Errorf("expected ErrUsed on second redeem, got %v", err)
	}
}

// TestRedeemExpired: вставка инвайта с прошедшим expires_at → ErrExpired.
func TestRedeemExpired(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := mkUser(t, ctx, pool, "redeem-expired")
	// TTL отрицательный → expires_at в прошлом.
	gen, err := repo.Generate(ctx, userID, uuid.Nil, -1*time.Hour)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, err = repo.Redeem(ctx, gen.RawToken)
	if err == nil || err.Error() != ErrExpired.Error() {
		t.Errorf("expected ErrExpired, got %v", err)
	}
}

// TestRedeemBadToken: формат не разбирается / случайный мусор → ErrBadToken
// или ErrNotFound. Здесь проверяем оба граничных случая.
func TestRedeemBadToken(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("без точки", func(t *testing.T) {
		_, err := repo.Redeem(ctx, "garbage-without-dot")
		if err == nil || err.Error() != ErrBadToken.Error() {
			t.Errorf("expected ErrBadToken, got %v", err)
		}
	})
	t.Run("несуществующий invite_id", func(t *testing.T) {
		fakeID := uuid.New()
		_, err := repo.Redeem(ctx, fakeID.String()+".whatever")
		if err == nil || err.Error() != ErrNotFound.Error() {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})
	t.Run("правильный invite_id, неверный raw", func(t *testing.T) {
		userID := mkUser(t, ctx, pool, "redeem-badraw")
		gen, _ := repo.Generate(ctx, userID, uuid.Nil, DefaultTTL)
		parts := strings.SplitN(gen.RawToken, ".", 2)
		// Подменяем raw на мусор.
		bad := parts[0] + ".tampered-raw-bytes"
		_, err := repo.Redeem(ctx, bad)
		// По дизайну (anti-enumeration) bcrypt-mismatch возвращает
		// ErrNotFound, чтобы не подсказывать про сам invite_id.
		if err == nil || err.Error() != ErrNotFound.Error() {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})
}

// TestGenerateEmitsOutbox: service.Generate должен emit'ить
// client_invite.generated.
func TestGenerateEmitsOutbox(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := mkUser(t, ctx, pool, "outbox-emit")
	_, err := svc.Generate(ctx, GenerateInput{UserID: userID})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM outbox
WHERE aggregate = $1 AND aggregate_id = $2 AND event_type = $3`,
		AggregateClientInvite, userID.String(), EventClientInviteGenerated,
	).Scan(&count); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if count != 1 {
		t.Errorf("outbox events = %d, want 1", count)
	}
}
