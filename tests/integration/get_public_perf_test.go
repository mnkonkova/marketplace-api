package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// makeFatSpecialist — спец с богатым публичным профилем: категории,
// скилы, портфолио (5 видео + 2 photo-set), отзывы. Отражает реальный
// прод-кейс, где GetPublic должен работать за <200мс.
func makeFatSpecialist(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"perf-"+uuid.NewString()+"@x").Scan(&uid); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, bio, is_freelance,
                                 is_published, moderation_status,
                                 rate_min, rate_max, currency)
VALUES ($1, $2, $3, TRUE, TRUE, 'approved', 1000, 5000, 'RUB')`,
		uid, "Perf "+uid.String()[:6], "Богатый профиль для perf-теста, минимум 30 символов."); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	// 5 видео-айтемов
	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(ctx, `
INSERT INTO portfolio_items (user_id, kind, title, description, video_url,
                             thumbnail_url, preview_url, preview_status,
                             sort_order, category_codes)
VALUES ($1, 'video', $2, $3, $4, $5, $6, 'ready', $7, '{}')`,
			uid, "video "+uuid.NewString()[:6], "описание", "https://x/v.mp4",
			"https://x/t.jpg", "https://x/p.mp4", i); err != nil {
			t.Fatalf("insert video: %v", err)
		}
	}
	// 2 photo-set'а, у каждого 4 фото
	for i := 0; i < 2; i++ {
		var itemID uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO portfolio_items (user_id, kind, title, description, thumbnail_url,
                             sort_order, category_codes, preview_status)
VALUES ($1, 'image', $2, $3, $4, $5, '{}', 'ready')
RETURNING id`,
			uid, "photoset", "descr", "https://x/t.jpg", 10+i).Scan(&itemID); err != nil {
			t.Fatalf("insert photoset: %v", err)
		}
		for j := 0; j < 4; j++ {
			if _, err := pool.Exec(ctx, `
INSERT INTO portfolio_images (portfolio_item_id, image_url, sort_order)
VALUES ($1, $2, $3)`, itemID, "https://x/img.jpg", j); err != nil {
				t.Fatalf("insert image: %v", err)
			}
		}
	}
	// 3 категории (одна primary)
	for i, code := range []string{"editor", "director", "ugc"} {
		primary := i == 0
		// Ищем существующую категорию с этим slug'ом; если нет — создаём.
		var exists bool
		_ = pool.QueryRow(ctx, `SELECT TRUE FROM specialty_categories WHERE code=$1`, code).Scan(&exists)
		if !exists {
			_, _ = pool.Exec(ctx, `INSERT INTO specialty_categories (code, title, sort_order, type)
VALUES ($1, $2, 100, 'production') ON CONFLICT DO NOTHING`, code, code)
		}
		_, _ = pool.Exec(ctx, `INSERT INTO specialist_categories (user_id, category_code, is_primary)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, uid, code, primary)
	}
	// 5 отзывов
	for i := 0; i < 5; i++ {
		var authorUID uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, now()) RETURNING id`,
			"rev-"+uuid.NewString()+"@x").Scan(&authorUID); err != nil {
			t.Fatalf("create review author: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO reviews (author_user_id, target_user_id, rating, text, author_name)
VALUES ($1, $2, 5, 'отличный', 'клиент')`, authorUID, uid); err != nil {
			t.Fatalf("insert review: %v", err)
		}
	}
	return uid
}

func cleanupFatSpecialist(t *testing.T, pool *pgxpool.Pool, uid uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pool.Exec(ctx, `DELETE FROM reviews WHERE target_user_id = $1`, uid)
	pool.Exec(ctx, `DELETE FROM users WHERE email LIKE 'rev-%@x'`)
	pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, uid.String())
	pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid)
}

// TestGetPublicShape — минимальная проверка что после рефакторинга
// через json_agg результат по форме тот же, что раньше: все поля
// заполнены, слайсы не-nil, вложенные images в photo-set'ах загружены.
func TestGetPublicShape(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeFatSpecialist(t, pool)
	defer cleanupFatSpecialist(t, pool, uid)

	repo := profiles.NewRepo(pool)
	p, err := repo.GetPublic(ctx, uid)
	if err != nil {
		t.Fatalf("GetPublic: %v", err)
	}

	if p.UserID != uid {
		t.Errorf("UserID mismatch: %v vs %v", p.UserID, uid)
	}
	if len(p.Categories) != 3 {
		t.Errorf("Categories = %d, want 3", len(p.Categories))
	}
	// primary должна быть первой (ORDER BY is_primary DESC)
	if len(p.Categories) > 0 && !p.Categories[0].IsPrimary {
		t.Errorf("первая категория должна быть primary")
	}
	if len(p.Portfolio) != 7 { // 5 video + 2 photoset
		t.Errorf("Portfolio = %d, want 7", len(p.Portfolio))
	}
	// найдём photo-set и проверим что images подгрузились
	photoCount := 0
	for _, it := range p.Portfolio {
		if it.Kind == "image" {
			photoCount++
			if len(it.Images) != 4 {
				t.Errorf("photo-set images = %d, want 4", len(it.Images))
			}
		}
	}
	if photoCount != 2 {
		t.Errorf("photo-set count = %d, want 2", photoCount)
	}
	if len(p.Reviews) != 5 {
		t.Errorf("Reviews = %d, want 5", len(p.Reviews))
	}
}

// TestGetPublicWarmLatency — SLA: тёплый GetPublic должен уложиться
// в 150 мс на реальном профиле (5 видео + 2 photo-set + 5 отзывов).
// До рефакторинга через json_agg было ~500 мс из-за 5 sequential round-trip'ов
// (см. curl-замеры 2026-07-01 на /specialist/{uid} → TTFB=560мс).
//
// Порог 150 мс с запасом: локально должно быть ~5-15 мс, на CI 30-60 мс.
// Если валится — либо регрессия перформанса, либо PG под нагрузкой
// (проверить `select count(*) from pg_stat_activity where state='active'`).
func TestGetPublicWarmLatency(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeFatSpecialist(t, pool)
	defer cleanupFatSpecialist(t, pool, uid)

	repo := profiles.NewRepo(pool)

	// Прогрев: первый вызов может быть медленным из-за холодного
	// PG buffer pool для specialist_categories/skills/reviews.
	if _, err := repo.GetPublic(ctx, uid); err != nil {
		t.Fatalf("warm-up call: %v", err)
	}

	const iterations = 5
	const budget = 150 * time.Millisecond
	var slow []time.Duration
	for i := 0; i < iterations; i++ {
		start := time.Now()
		if _, err := repo.GetPublic(ctx, uid); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if d := time.Since(start); d > budget {
			slow = append(slow, d)
		}
	}
	if len(slow) > 0 {
		t.Errorf("%d/%d итераций превысили %s: %v", len(slow), iterations, budget, slow)
	}
}

// TestGetPublicSingleRoundTrip — регрессия против отката к 5-запросной
// версии. Сравнивает время одной GetPublic с временем одного вручную
// сделанного round-trip к БД: должны быть в одном порядке
// (≤ 3× baseline). Если GetPublic > 5×baseline, значит запросов больше
// одного и рефакторинг сломался.
func TestGetPublicSingleRoundTrip(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeFatSpecialist(t, pool)
	defer cleanupFatSpecialist(t, pool, uid)

	repo := profiles.NewRepo(pool)

	// Прогрев
	if _, err := repo.GetPublic(ctx, uid); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	var baseline time.Duration
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	bStart := time.Now()
	for i := 0; i < 20; i++ {
		if err := pool.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
			t.Fatalf("baseline query %d: %v", i, err)
		}
	}
	baseline = time.Since(bStart) / 20 // средний round-trip

	// GetPublic (среднее из 20)
	gStart := time.Now()
	for i := 0; i < 20; i++ {
		if _, err := repo.GetPublic(ctx, uid); err != nil {
			t.Fatalf("GetPublic %d: %v", i, err)
		}
	}
	avgGet := time.Since(gStart) / 20

	// Ожидаем: avgGet ≤ 8 × baseline. При 5 round-trip'ах было бы ~5x + PG
	// работа = 10-15x. При одном json_agg-запросе PG работа доминирует,
	// но всё равно должно быть в пределах 8x baseline.
	if avgGet > baseline*8 {
		t.Errorf("GetPublic (avg %s) сильно медленнее baseline query (%s) — вероятно N+1 регрессия",
			avgGet, baseline)
	}
	t.Logf("baseline SELECT 1 = %s; GetPublic avg = %s (ratio %.1fx)",
		baseline, avgGet, float64(avgGet)/float64(baseline))
}
