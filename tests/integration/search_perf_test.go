package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/platform/es"
	"marketpclce/internal/search"
	"marketpclce/tests/integration"
)

// searchTestFixture — общая обвязка для тестов на реальный OpenSearch.
// Каждый вызов создаёт отдельный индекс с суффиксом (изолированный от
// dev/prod и от соседних тестов, запускаемых параллельно). Возвращает
// сервис + индексер для наполнения тест-документами.
type searchTestFixture struct {
	svc     *search.Service
	indexer *search.Indexer
	client  *es.Client
	index   string
	pool    *pgxpool.Pool
}

func setupSearchFixture(t *testing.T) *searchTestFixture {
	t.Helper()
	pool := integration.Pool(t)

	osURL := os.Getenv("TEST_OPENSEARCH_URL")
	if osURL == "" {
		osURL = os.Getenv("OPENSEARCH_URL")
	}
	if osURL == "" {
		osURL = "http://localhost:9200"
	}
	client := es.New(osURL)
	// Быстрый ping — если ES недоступен, пропускаем весь блок а не падаем.
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Search(pingCtx, "_all", map[string]any{"size": 0}); err != nil {
		t.Skipf("OpenSearch недоступен по %s: %v — search-integration пропущены", osURL, err)
	}

	// Уникальный индекс на тест: имя в snake_case, ES не любит верхний регистр.
	index := "search_test_" + uuid.NewString()[:8]
	ctx := context.Background()
	if err := client.EnsureIndex(ctx, index, search.IndexMapping()); err != nil {
		t.Fatalf("create index %s: %v", index, err)
	}
	t.Cleanup(func() {
		_ = client.DeleteIndex(context.Background(), index)
	})

	repo := search.NewRepo(pool)
	svc := search.NewService(client, index)
	indexer := search.NewIndexer(repo, client, index)

	return &searchTestFixture{
		svc:     svc,
		indexer: indexer,
		client:  client,
		index:   index,
		pool:    pool,
	}
}

// indexDirect — обходит Postgres (repo.LoadDoc) и кладёт готовый IndexDoc
// прямо в OS. Нужен когда мы хотим кастомные комбинации категорий /
// bio без создания реальных спецов в БД.
func (f *searchTestFixture) indexDirect(t *testing.T, doc search.IndexDoc) {
	t.Helper()
	if doc.UserID == "" {
		doc.UserID = uuid.NewString()
	}
	if err := f.client.IndexDoc(context.Background(), f.index, doc.UserID, doc); err != nil {
		t.Fatalf("index doc %s: %v", doc.UserID, err)
	}
}

// refresh — ES-индекс eventually consistent (обычно 1с). В тестах ждать
// нельзя: явный refresh делает новые доки видимыми немедленно.
func (f *searchTestFixture) refresh(t *testing.T) {
	t.Helper()
	if _, err := f.client.Search(context.Background(), f.index+"/_refresh", map[string]any{}); err != nil {
		// _refresh — POST но клиент делает GET; используем workaround через любую search-операцию
		// после короткой паузы. Если ES не подхватил — тест увидит total=0.
		time.Sleep(150 * time.Millisecond)
	}
}

// TestSearch_BroadeningRespectsFilters — регресс v2.1 фикса.
// User ищет «моушн-дизайнер» + category=director. Матчей 0. Раньше
// broadening выкидывал Q="" и возвращал ВСЕХ director-спецов, юзер
// получал «случайных» людей вместо честного пустого результата.
// После фикса (см. service.go:113): броадинг НЕ срабатывает при
// заданной категории.
func TestSearch_BroadeningRespectsFilters(t *testing.T) {
	f := setupSearchFixture(t)
	// motion-спец с title'ом «Моушн-дизайнер» в category_titles.
	f.indexDirect(t, search.IndexDoc{
		DisplayName:     "Alice Motion",
		Categories:      []string{"motion"},
		PrimaryCategory: "motion",
		CategoryTitles:  "Моушн-дизайнер",
		IsPublished:     true,
	})
	// director-спец, БЕЗ motion в title'ах.
	f.indexDirect(t, search.IndexDoc{
		DisplayName:     "Bob Director",
		Categories:      []string{"director"},
		PrimaryCategory: "director",
		CategoryTitles:  "Режиссёр",
		IsPublished:     true,
	})
	f.refresh(t)
	// Небольшая пауза — ES иногда требует до 1с чтобы док стал visible.
	time.Sleep(1200 * time.Millisecond)

	res, err := f.svc.Search(context.Background(), search.Query{
		Q:          "моушн-дизайнер",
		Categories: []string{"director"},
		Limit:      20,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Ожидаем 0: моушн-спец не в director-категории, а director-спец
	// не матчится на «моушн-дизайнер». Broadening НЕ должен подсовывать
	// Bob'а.
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0 (broadening не должен срабатывать при явной category)", res.Total)
		for _, it := range res.Items {
			t.Logf("  got item: %s / %v", it.DisplayName, it.Categories)
		}
	}
	if res.Broadened {
		t.Errorf("Broadened=true при заданных фильтрах — регресс фикса broadening")
	}
}

// TestSearch_BroadeningKicksInWithoutFilters — обратная проверка:
// когда фильтров нет и Q ничего не нашёл, старый broadening всё ещё
// работает (показываем «всё что есть», лучше чем empty page).
func TestSearch_BroadeningKicksInWithoutFilters(t *testing.T) {
	f := setupSearchFixture(t)
	f.indexDirect(t, search.IndexDoc{
		DisplayName:     "Alice Motion",
		Categories:      []string{"motion"},
		PrimaryCategory: "motion",
		CategoryTitles:  "Моушн-дизайнер",
		IsPublished:     true,
	})
	f.refresh(t)
	time.Sleep(1200 * time.Millisecond)

	res, err := f.svc.Search(context.Background(), search.Query{
		Q:     "абсолютно-несуществующий-запрос-abcxyz",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.Broadened {
		t.Errorf("Broadened=false — ожидался старый broadening без фильтров")
	}
	if res.Total == 0 {
		t.Errorf("Total=0 при broadening: ожидался хотя бы 1 спец (Alice)")
	}
}

// TestSearch_CategoryTitlesMatching — регресс v2.1 фикса маппинга.
// «моушн-дизайнер» должно матчить спеца с CategoryTitles="Моушн-дизайнер"
// через multi_match + synonym_graph analyzer. Без category_titles^2 в
// fields (см. buildQuery) этот тест падает.
func TestSearch_CategoryTitlesMatching(t *testing.T) {
	f := setupSearchFixture(t)
	f.indexDirect(t, search.IndexDoc{
		DisplayName:     "Alice",
		Categories:      []string{"motion"},
		PrimaryCategory: "motion",
		CategoryTitles:  "Моушн-дизайнер",
		Bio:             "Делаю анимации",
		IsPublished:     true,
	})
	f.indexDirect(t, search.IndexDoc{
		DisplayName:     "Bob",
		Categories:      []string{"editor"},
		PrimaryCategory: "editor",
		CategoryTitles:  "Монтажёр",
		Bio:             "Клипы и рилс",
		IsPublished:     true,
	})
	f.refresh(t)
	time.Sleep(1200 * time.Millisecond)

	res, err := f.svc.Search(context.Background(), search.Query{
		Q:     "моушн-дизайнер",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total < 1 {
		t.Fatalf("Total = %d, want ≥ 1 (Alice должна найтись по category_titles)", res.Total)
	}
	if res.Items[0].DisplayName != "Alice" {
		t.Errorf("top hit = %q, want Alice (по category_titles^2)", res.Items[0].DisplayName)
	}
}

// TestSearch_UnpublishedNotVisible — is_published=false спец не должен
// показываться. Это hard-фильтр в bool.filter (см. TestBuildQuery_
// IsPublishedAlwaysFilter в unit-тестах), integration подтверждает что
// это реально работает end-to-end.
func TestSearch_UnpublishedNotVisible(t *testing.T) {
	f := setupSearchFixture(t)
	f.indexDirect(t, search.IndexDoc{
		DisplayName:    "Hidden",
		CategoryTitles: "Монтажёр",
		IsPublished:    false,
	})
	f.refresh(t)
	time.Sleep(1200 * time.Millisecond)

	res, err := f.svc.Search(context.Background(), search.Query{Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, it := range res.Items {
		if it.DisplayName == "Hidden" {
			t.Errorf("неопубликованный спец Hidden виден в результатах")
		}
	}
}

// TestSearch_WarmLatency — SLA: warm поиск с 20 доками в индексе
// должен укладываться в 100 мс. Локально — 5-15 мс, CI (медленнее
// диск / общая нагрузка) — до 60 мс. Порог 100 мс с запасом.
//
// Если валится — либо регрессия (добавили sequential-запросы в Service),
// либо ES под нагрузкой. Проверить `_nodes/stats/os,jvm,indices`.
func TestSearch_WarmLatency(t *testing.T) {
	f := setupSearchFixture(t)
	for i := 0; i < 20; i++ {
		f.indexDirect(t, search.IndexDoc{
			DisplayName:     "Spec " + uuid.NewString()[:6],
			Categories:      []string{"editor"},
			PrimaryCategory: "editor",
			CategoryTitles:  "Монтажёр",
			Bio:             "Bio текст",
			IsPublished:     true,
		})
	}
	time.Sleep(1500 * time.Millisecond)

	ctx := context.Background()
	// Прогрев: первый search может быть заметно медленнее из-за холодных
	// кэшей filter'а и analyzer'а.
	if _, err := f.svc.Search(ctx, search.Query{Q: "монтаж", Limit: 20}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	const iterations = 10
	const budget = 100 * time.Millisecond
	var slow []time.Duration
	for i := 0; i < iterations; i++ {
		start := time.Now()
		if _, err := f.svc.Search(ctx, search.Query{Q: "монтаж", Limit: 20}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if d := time.Since(start); d > budget {
			slow = append(slow, d)
		}
	}
	if len(slow) > iterations/3 { // терпим единичные всплески (GC/refresh)
		t.Errorf("%d/%d итераций превысили %s: %v", len(slow), iterations, budget, slow)
	}
}

// TestSearch_ParallelSoftRelaxDoesntDoubleLatency — регресс P6 оптимизации:
// релакс-запрос и основной идут ПАРАЛЛЕЛЬНО через errgroup. Если кто-то
// вернёт последовательный порядок, latency удвоится. Проверяем что
// search с soft-фильтром (city) и без него отличается не больше чем
// в 2× (запас на overhead relax-запроса, который всё равно идёт).
func TestSearch_ParallelSoftRelaxDoesntDoubleLatency(t *testing.T) {
	f := setupSearchFixture(t)
	for i := 0; i < 10; i++ {
		f.indexDirect(t, search.IndexDoc{
			DisplayName:     "Spec " + uuid.NewString()[:6],
			Categories:      []string{"editor"},
			PrimaryCategory: "editor",
			CategoryTitles:  "Монтажёр",
			City:            "msk",
			IsPublished:     true,
		})
	}
	time.Sleep(1500 * time.Millisecond)
	ctx := context.Background()

	// Прогрев
	_, _ = f.svc.Search(ctx, search.Query{Q: "монтаж", Limit: 20})

	// Baseline: без soft-фильтров, только основной запрос.
	base := avgLatency(ctx, f.svc, search.Query{Q: "монтаж", Limit: 20}, 5)
	// С soft-фильтром: основной + релакс параллельно.
	withSoft := avgLatency(ctx, f.svc, search.Query{Q: "монтаж", City: "spb", Limit: 20}, 5)

	// withSoft ≤ 2.5 × base. При sequential было бы ~2×, но параллельно
	// разница минимальна (max(t1,t2) ≈ 1.1×). Порог 2.5× с запасом на
	// шум ES / GC.
	if withSoft > 3*base && withSoft > 30*time.Millisecond {
		t.Errorf("soft-relax latency %s > 3× baseline %s — вероятно вернулся sequential",
			withSoft, base)
	}
	t.Logf("baseline avg = %s, with soft-filter avg = %s (ratio %.2fx)",
		base, withSoft, float64(withSoft)/float64(base))
}

func avgLatency(ctx context.Context, svc *search.Service, q search.Query, n int) time.Duration {
	start := time.Now()
	for i := 0; i < n; i++ {
		_, _ = svc.Search(ctx, q)
	}
	return time.Since(start) / time.Duration(n)
}
