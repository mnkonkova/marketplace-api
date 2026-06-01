package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/leads"
	"marketpclce/tests/integration"
)

// makeSpecialist — создаёт user+specialist_profiles, отдаёт user_id.
func makeSpecialist(t *testing.T, pool *pgxpool.Pool, published bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, role, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', 'client', TRUE, now()) RETURNING id`,
		"sp-"+uuid.NewString()+"@x").Scan(&uid); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, is_published)
VALUES ($1, $2, $3)`, uid, "Sp "+uid.String()[:6], published); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	return uid
}

// ---- ТЕСТ: бриф со всеми опубликованными — ок ----

func TestLeadCreateAllPublishedOK(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	sp1 := makeSpecialist(t, pool, true)
	sp2 := makeSpecialist(t, pool, true)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{sp1, sp2})

	svc := leads.NewService(leads.NewRepo(pool))
	res, err := svc.Create(ctx, leads.CreateInput{
		ClientName:    "Test",
		ClientContact: "+79991234567",
		Brief:         "бриф с двумя опубликованными",
		SpecialistIDs: []uuid.UUID{sp1, sp2},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.ID == uuid.Nil {
		t.Errorf("empty lead id")
	}
	defer pool.Exec(ctx, `DELETE FROM leads WHERE id=$1`, res.ID)
}

// ---- ТЕСТ: хоть один не-опубликованный → ErrSpecialistUnpublished ----

func TestLeadCreateAnyUnpublishedRejects(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	sp1 := makeSpecialist(t, pool, true)
	sp2 := makeSpecialist(t, pool, false) // не опубликован
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{sp1, sp2})

	svc := leads.NewService(leads.NewRepo(pool))
	_, err := svc.Create(ctx, leads.CreateInput{
		ClientName:    "Test",
		ClientContact: "+79991234567",
		Brief:         "бриф со скрытым спецом длинной более 10",
		SpecialistIDs: []uuid.UUID{sp1, sp2},
	})
	if !errors.Is(err, leads.ErrSpecialistUnpublished) {
		t.Errorf("ожидалась ErrSpecialistUnpublished, получено %v", err)
	}

	// Лида не должно быть в БД.
	var n int
	_ = pool.QueryRow(ctx, `
SELECT COUNT(*) FROM leads
WHERE brief = 'бриф со скрытым спецом длинной более 10'`).Scan(&n)
	if n != 0 {
		t.Errorf("в БД записался лид при unpublished получателе: %d", n)
	}
}

// ---- ТЕСТ: несуществующий UUID = такой же реджект ----

func TestLeadCreateNonExistentRejects(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	sp := makeSpecialist(t, pool, true)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, sp)

	svc := leads.NewService(leads.NewRepo(pool))
	_, err := svc.Create(ctx, leads.CreateInput{
		ClientName:    "Test",
		ClientContact: "+79991234567",
		Brief:         "бриф с несуществующим UUID длиннее 10",
		SpecialistIDs: []uuid.UUID{sp, uuid.New()},
	})
	if !errors.Is(err, leads.ErrSpecialistUnpublished) {
		t.Errorf("ожидалась ErrSpecialistUnpublished, получено %v", err)
	}
}
