package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// ─── Helpers ──────────────────────────────────────────────────────

// makeFeaturedSpec — спец с профилем, без категорий (для featured они не
// нужны: закрепление не валидирует контент работы).
func makeFeaturedSpec(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"feat-"+uuid.NewString()+"@x").Scan(&uid); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, bio, is_freelance, is_published)
VALUES ($1, $2, $3, TRUE, FALSE)`,
		uid, "Feat "+uid.String()[:6], "Bio for featured test specialist."); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	return uid
}

// insertVideoItem — видео-работа напрямую в БД. Мимо сервиса, потому что
// AddPortfolioVideo тянет за собой валидацию S3-ключей и категорий, которые
// к закреплению отношения не имеют.
func insertVideoItem(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, title string, sortOrder int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
INSERT INTO portfolio_items (user_id, title, description, video_url, kind, category_codes, sort_order)
VALUES ($1, $2, '', 'https://stub.test/v/' || gen_random_uuid() || '.mp4', 'video', '{}', $3)
RETURNING id`, userID, title, sortOrder).Scan(&id); err != nil {
		t.Fatalf("insert video item: %v", err)
	}
	return id
}

func isFeatured(t *testing.T, pool *pgxpool.Pool, itemID uuid.UUID) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(),
		`SELECT is_featured FROM portfolio_items WHERE id = $1`, itemID).Scan(&v); err != nil {
		t.Fatalf("read is_featured: %v", err)
	}
	return v
}

func newFeaturedSvc(t *testing.T) *profiles.Service {
	return profiles.NewService(profiles.NewRepo(integration.Pool(t)))
}

// ─── SetPortfolioFeatured ─────────────────────────────────────────

func TestFeatured_SetAndUnset(t *testing.T) {
	pool := integration.Pool(t)
	uid := makeFeaturedSpec(t, pool)
	item := insertVideoItem(t, pool, uid, "Промо-ролик", 0)
	svc := newFeaturedSvc(t)

	got, err := svc.SetPortfolioFeatured(context.Background(), uid, item, true)
	if err != nil {
		t.Fatalf("set featured: %v", err)
	}
	if !got.IsFeatured {
		t.Error("ответ сервиса: is_featured=false после закрепления")
	}
	if !isFeatured(t, pool, item) {
		t.Error("в БД is_featured=false после закрепления")
	}

	got, err = svc.SetPortfolioFeatured(context.Background(), uid, item, false)
	if err != nil {
		t.Fatalf("unset featured: %v", err)
	}
	if got.IsFeatured || isFeatured(t, pool, item) {
		t.Error("закрепление не снялось")
	}
}

// Главный инвариант: закреплённая работа у специалиста ровно одна. Без
// снятия флага с предыдущей мы бы упёрлись в partial unique index (или,
// без него, показали два флагмана на публичной странице).
func TestFeatured_OnlyOnePerSpecialist(t *testing.T) {
	pool := integration.Pool(t)
	uid := makeFeaturedSpec(t, pool)
	first := insertVideoItem(t, pool, uid, "Первая", 0)
	second := insertVideoItem(t, pool, uid, "Вторая", 1)
	svc := newFeaturedSvc(t)
	ctx := context.Background()

	if _, err := svc.SetPortfolioFeatured(ctx, uid, first, true); err != nil {
		t.Fatalf("feature first: %v", err)
	}
	if _, err := svc.SetPortfolioFeatured(ctx, uid, second, true); err != nil {
		t.Fatalf("feature second: %v", err)
	}

	if isFeatured(t, pool, first) {
		t.Error("у первой работы остался флаг закрепления")
	}
	if !isFeatured(t, pool, second) {
		t.Error("вторая работа не закрепилась")
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM portfolio_items WHERE user_id = $1 AND is_featured`, uid).Scan(&n); err != nil {
		t.Fatalf("count featured: %v", err)
	}
	if n != 1 {
		t.Errorf("закреплённых работ %d, ожидали 1", n)
	}
}

// Чужую работу закрепить нельзя: WHERE user_id = $2 не найдёт строку.
func TestFeatured_ForeignItemNotFound(t *testing.T) {
	pool := integration.Pool(t)
	owner := makeFeaturedSpec(t, pool)
	stranger := makeFeaturedSpec(t, pool)
	item := insertVideoItem(t, pool, owner, "Чужая", 0)
	svc := newFeaturedSvc(t)

	_, err := svc.SetPortfolioFeatured(context.Background(), stranger, item, true)
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("ожидали ErrNotFound, получили %v", err)
	}
	if isFeatured(t, pool, item) {
		t.Error("чужая работа оказалась закреплена")
	}
}

// Закрепление одной работы не должно снимать флаг у другого специалиста —
// UPDATE ограничен user_id.
func TestFeatured_DoesNotTouchOtherSpecialists(t *testing.T) {
	pool := integration.Pool(t)
	first := makeFeaturedSpec(t, pool)
	second := makeFeaturedSpec(t, pool)
	firstItem := insertVideoItem(t, pool, first, "Работа А", 0)
	secondItem := insertVideoItem(t, pool, second, "Работа Б", 0)
	svc := newFeaturedSvc(t)
	ctx := context.Background()

	if _, err := svc.SetPortfolioFeatured(ctx, first, firstItem, true); err != nil {
		t.Fatalf("feature first spec: %v", err)
	}
	if _, err := svc.SetPortfolioFeatured(ctx, second, secondItem, true); err != nil {
		t.Fatalf("feature second spec: %v", err)
	}

	if !isFeatured(t, pool, firstItem) {
		t.Error("закрепление у первого спеца слетело после действий второго")
	}
	if !isFeatured(t, pool, secondItem) {
		t.Error("второй спец не получил закрепление")
	}
}
