package integration_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/platform/es"
	"marketpclce/internal/search"
	"marketpclce/tests/integration"
)

// Призрак в индексе: документ человека, которого в базе больше нет.
//
// Пользователей удаляют и МИМО приложения — этот самый харнесс сносит
// своих одним `DELETE FROM users`, и outbox при этом молчит. Документ
// остаётся в индексе навсегда, каталог показывает карточку с именем и
// фотографией, а `POST /me/orders` отвечает на неё 409 not_a_creator:
// «в пакет берутся только блогеры и авторы UGC». Заказчик видит, что
// система сама предложила человека и сама же отказала.
//
// Проверяем обе стороны уборки, и вторая важнее первой: призрак уходит,
// а ЖИВОЙ остаётся. Ошибка «удалили лишнего» здесь выкашивает каталог
// целиком и выглядит точно так же — пустой выдачей.
func TestIndexGhostSweep(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	osURL := os.Getenv("TEST_OPENSEARCH_URL")
	if osURL == "" {
		osURL = os.Getenv("OPENSEARCH_URL")
	}
	if osURL == "" {
		osURL = "http://localhost:9200"
	}
	client := es.New(osURL)
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := client.Search(pingCtx, "_all", map[string]any{"size": 0}); err != nil {
		t.Skipf("OpenSearch недоступен по %s: %v — уборка призраков пропущена", osURL, err)
	}

	index := "ghosts_test_" + uuid.NewString()[:8]
	if err := client.CreateIndex(ctx, index, search.IndexMapping()); err != nil {
		t.Fatalf("create index: %v", err)
	}
	t.Cleanup(func() { _ = client.DeleteIndex(context.Background(), index) })

	// Живой и призрак. Живого заводим полностью — с профилем и
	// категорией, как настоящего; призрака сносим из users сразу после
	// того, как его документ лёг в индекс.
	mk := func(name string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, is_active, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, TRUE, now()) RETURNING id`,
			"ghost-"+uuid.NewString()+"@example.com").Scan(&id); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, is_published, moderation_status)
VALUES ($1, $2, TRUE, 'approved')`, id, name); err != nil {
			t.Fatalf("create profile: %v", err)
		}
		return id
	}
	alive := mk("Живой креатор")
	ghost := mk("Призрак каталога")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, alive)
	})

	indexer := search.NewIndexer(search.NewRepo(pool), client, index)
	for _, id := range []uuid.UUID{alive, ghost} {
		if err := indexer.Reconcile(ctx, id, 0); err != nil {
			t.Fatalf("index %s: %v", id, err)
		}
	}
	// Тем же способом, каким это делают харнесс и фикстуры: мимо
	// приложения, без outbox. Документ остаётся.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ghost); err != nil {
		t.Fatalf("delete ghost user: %v", err)
	}
	refresh(t, osURL, index)

	removed, err := indexer.SweepGhosts(ctx)
	if err != nil {
		t.Fatalf("SweepGhosts: %v", err)
	}
	if removed != 1 {
		t.Errorf("удалено %d документов, ожидали ровно один — призрака", removed)
	}
	refresh(t, osURL, index)

	left := docIDs(t, client, index)
	if left[ghost.String()] {
		t.Error("призрак остался в индексе — каталог снова предложит человека, которого нет")
	}
	if !left[alive.String()] {
		t.Fatal("уборка снесла живого — это хуже призрака: из каталога пропадают настоящие люди")
	}
}

// refresh — дождаться, пока индекс увидит свои же записи.
//
// Прямым HTTP, мимо клиента: у него нет метода refresh, и заводить его
// ради теста незачем — приложению обновление по требованию не нужно, а
// тесту без него _search отдаёт пустой снимок, и проверка становится
// зелёной на пустом месте.
func refresh(t *testing.T, osURL, index string) {
	t.Helper()
	resp, err := http.Post(osURL+"/"+index+"/_refresh", "application/json", nil)
	if err != nil {
		t.Fatalf("refresh %s: %v", index, err)
	}
	_ = resp.Body.Close()
}

func docIDs(t *testing.T, client *es.Client, index string) map[string]bool {
	t.Helper()
	resp, err := client.Search(context.Background(), index, map[string]any{
		"size":    100,
		"_source": false,
		"query":   map[string]any{"match_all": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("scan index: %v", err)
	}
	out := make(map[string]bool, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		out[h.ID] = true
	}
	return out
}
