package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/tests/integration"
)

// Медиана просмотров креатора — «сколько он обычно даёт за ролик».
//
// Среднее здесь врало бы: один залетевший ролик поднимает его вдвое и
// обещает заказчику то, чего обычно не бывает. Проверяем именно
// медиану, поэтому ряд подобран так, что среднее и медиана расходятся.
func TestCreatorMedianIsMedianNotAverage(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "median")
	defer cleanup()

	var creator uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT creator_user_id FROM project_publications WHERE project_id = $1 LIMIT 1`,
		pid).Scan(&creator); err != nil {
		t.Fatalf("креатор: %v", err)
	}

	// Ряд 100k / 200k / 5 000k: медиана 200 000, среднее — 1 766 667.
	views := []int64{100_000, 200_000, 5_000_000}
	for i, v := range views {
		pub := creatorPubWithViews(t, pool, pid, creator, i, v)
		_ = pub
	}

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET assigned_to_user_id = $1 WHERE id = $2`, manager, pid); err != nil {
		t.Fatalf("назначение менеджера: %v", err)
	}

	code, body := h.Do(t, http.MethodGet,
		"/api/v1/manager/projects/"+pid.String()+"/creators", h.Token(t, manager), nil)
	if code != http.StatusOK {
		t.Fatalf("состав проекта: код %d, тело %v", code, body)
	}
	items, _ := body["items"].([]any)
	var seen bool
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["user_id"] != creator.String() {
			continue
		}
		seen = true
		med, _ := m["median"].(map[string]any)
		if med == nil {
			t.Fatalf("медианы нет у креатора с тремя измеренными роликами: %v", m)
		}
		if got := num(t, med, "views"); got != 200_000 {
			t.Errorf("медиана %d, ожидалось 200 000 (среднее было бы 1 766 667)", got)
		}
		if got := num(t, med, "basis"); got != 3 {
			t.Errorf("медиана посчитана по %d роликам, ожидалось 3", got)
		}
	}
	if !seen {
		t.Fatalf("креатора нет в составе: %v", body)
	}
}

// Двух роликов мало: полусумма — не медиана, и выдавать её за «обычно
// столько» нельзя. Поля нет вовсе, чтобы интерфейс мог не рисовать
// подпись, а не рисовать её с непроверяемым числом.
func TestCreatorMedianNeedsEnoughVideos(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "median2")
	defer cleanup()

	var creator uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT creator_user_id FROM project_publications WHERE project_id = $1 LIMIT 1`,
		pid).Scan(&creator); err != nil {
		t.Fatalf("креатор: %v", err)
	}
	creatorPubWithViews(t, pool, pid, creator, 0, 100_000)
	creatorPubWithViews(t, pool, pid, creator, 1, 300_000)

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET assigned_to_user_id = $1 WHERE id = $2`, manager, pid); err != nil {
		t.Fatalf("назначение менеджера: %v", err)
	}

	code, body := h.Do(t, http.MethodGet,
		"/api/v1/manager/projects/"+pid.String()+"/creators", h.Token(t, manager), nil)
	if code != http.StatusOK {
		t.Fatalf("состав проекта: код %d, тело %v", code, body)
	}
	items, _ := body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["user_id"] != creator.String() {
			continue
		}
		if _, ok := m["median"]; ok {
			t.Errorf("по двум роликам медианы быть не должно: %v", m["median"])
		}
	}
}

// creatorPubWithViews — выкладка креатора с одной ссылкой и снимком
// статистики. Ровно то, из чего складывается медиана.
func creatorPubWithViews(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID,
	idx int, views int64) uuid.UUID {

	t.Helper()
	ctx := context.Background()
	var pubID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO project_publications (project_id, creator_user_id, due_date, status, title)
VALUES ($1, $2, CURRENT_DATE - $3::int, 'done', $4)
RETURNING id`, projectID, creator, idx+1, "Ролик "+uuid.NewString()[:6]).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO publication_links (publication_id, platform, url, url_canonical, submitted_at)
VALUES ($1, 'tiktok', $2, $2, now()) RETURNING id`,
		pubID, "https://tiktok.com/@x/video/"+uuid.NewString()[:8]).Scan(&linkID); err != nil {
		t.Fatalf("ссылка: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, collected_at)
VALUES ($1, CURRENT_DATE, $2, 0, 0, now())`, linkID, views); err != nil {
		t.Fatalf("статистика: %v", err)
	}
	return pubID
}
