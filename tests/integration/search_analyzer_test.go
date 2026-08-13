package integration_test

import (
	"context"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/platform/es"
	"marketpclce/internal/search"
)

// Регресс-тесты на анализатор ru_en (char_filter «ё»→«е» + ru_en_synonyms).
//
// Баг, из-за которого тест появился: запрос «монтажер» (через «е») давал 0
// хитов, потому что в профилях категория записана «Монтажёр». Пустая выдача
// включала broadening (search.Service выкидывает q и возвращает всех), и на
// запрос «монтажер» первым показывался актёр. См. docs/REINDEX.md — правки
// словаря требуют переиндексации, поэтому важно ловить регрессы до деплоя.
//
// В отличие от search_perf_test.go этой обвязке НЕ нужен Postgres: документы
// кладём в индекс напрямую, минуя Repo.LoadDoc.

type analyzerFixture struct {
	svc    *search.Service
	client *es.Client
	index  string
	url    string
}

func setupAnalyzerFixture(t *testing.T) *analyzerFixture {
	t.Helper()

	osURL := os.Getenv("TEST_OPENSEARCH_URL")
	if osURL == "" {
		osURL = os.Getenv("OPENSEARCH_URL")
	}
	if osURL == "" {
		osURL = "http://localhost:9200"
	}
	client := es.New(osURL)
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Search(pingCtx, "_all", map[string]any{"size": 0}); err != nil {
		t.Skipf("OpenSearch недоступен по %s: %v — analyzer-тесты пропущены", osURL, err)
	}

	index := "analyzer_test_" + uuid.NewString()[:8]
	ctx := context.Background()
	if err := client.CreateIndex(ctx, index, search.IndexMapping()); err != nil {
		t.Fatalf("create index %s: %v", index, err)
	}
	t.Cleanup(func() { _ = client.DeleteIndex(context.Background(), index) })

	return &analyzerFixture{svc: search.NewService(client, index), client: client, index: index, url: osURL}
}

func (f *analyzerFixture) index_(t *testing.T, docs ...search.IndexDoc) {
	t.Helper()
	ctx := context.Background()
	for _, d := range docs {
		if err := f.client.IndexDoc(ctx, f.index, d.UserID, d); err != nil {
			t.Fatalf("index %s: %v", d.UserID, err)
		}
	}
	// es.Client индексирует с refresh=false и не умеет POST /_refresh —
	// дёргаем его напрямую, иначе документы не видны следующему запросу.
	resp, err := http.Post(f.url+"/"+f.index+"/_refresh", "application/json", nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status %d", resp.StatusCode)
	}
}

// doc — минимальный опубликованный спец. Поля, которых нет в кейсе,
// оставляем пустыми: анализатор нас интересует, а не карточка.
func doc(id, name, categoryTitles, bio string, categories ...string) search.IndexDoc {
	return search.IndexDoc{
		UserID:      id,
		DisplayName: name,
		Bio:         bio,
		// В проде это string_agg(sc.title) из specialty_categories — там
		// названия именно с «ё»: «Монтажёр», «Актёр», «Режиссёр».
		CategoryTitles: categoryTitles,
		Categories:     categories,
		IsPublished:    true,
	}
}

// TestAnalyzer_CategoryQueries — «человеческий» запрос по названию профессии
// должен находить спеца этой категории, независимо от «ё»/«е», падежа,
// числа и языка. И НЕ должен находить чужую категорию.
func TestAnalyzer_CategoryQueries(t *testing.T) {
	f := setupAnalyzerFixture(t)

	const (
		editorID   = "11111111-1111-1111-1111-111111111111"
		actorID    = "22222222-2222-2222-2222-222222222222"
		directorID = "33333333-3333-3333-3333-333333333333"
		targetID   = "44444444-4444-4444-4444-444444444444"
		motionID   = "55555555-5555-5555-5555-555555555555"
	)

	f.index_(t,
		doc(editorID, "Ксения", "Монтажёр", "", "editor"),
		// bio с «рекламе» — из-за бареного «реклама» в словаре синонимов
		// актёр раньше попадал в выдачу по «таргет».
		doc(actorID, "Максим", "Актёр", "Профессиональный экшн-актёр, снимаюсь в рекламе и кино", "actor"),
		doc(directorID, "Оксана", "Режиссёр", "Клипы, короткометражки", "director"),
		doc(targetID, "Пётр", "Таргет + SEO", "Настраиваю рекламные кабинеты", "ads_seo"),
		doc(motionID, "Влад", "Моушн-дизайнер", "", "motion"),
	)

	cases := []struct {
		q    string
		want []string
	}{
		// Собственно баг: «е» вместо «ё» в запросе.
		{"монтажер", []string{editorID}},
		{"монтажёр", []string{editorID}},
		// Словоформы и родственные слова.
		{"монтажеры", []string{editorID}},
		{"монтаж", []string{editorID}},
		{"видеомонтаж", []string{editorID}},
		{"смонтировать", []string{editorID}},
		// Английский эквивалент.
		{"editor", []string{editorID}},
		{"video editing", []string{editorID}},
		// Остальные категории — оба написания.
		{"актер", []string{actorID}},
		{"актёр", []string{actorID}},
		{"актриса", []string{actorID}},
		{"actor", []string{actorID}},
		{"каскадер", []string{actorID}},
		{"режиссер", []string{directorID}},
		{"режиссёр", []string{directorID}},
		{"режиссура", []string{directorID}},
		{"моушн дизайнер", []string{motionID}},
		{"моушн-дизайнер", []string{motionID}},
		{"motion designer", []string{motionID}},
		{"анимация", []string{motionID}},
		// «таргет» не должен вытаскивать актёра со словом «рекламе» в bio.
		{"таргет", []string{targetID}},
		{"таргетолог", []string{targetID}},
		{"seo", []string{targetID}},
	}

	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			res, err := f.svc.Search(context.Background(), search.Query{Q: tc.q, Limit: 20})
			if err != nil {
				t.Fatalf("search %q: %v", tc.q, err)
			}
			if res.Broadened {
				t.Fatalf("запрос %q дал 0 хитов и ушёл в broadening — словарь не покрывает слово", tc.q)
			}
			got := make([]string, 0, len(res.Items))
			for _, it := range res.Items {
				got = append(got, it.UserID)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("запрос %q → %v, want %v", tc.q, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("запрос %q → %v, want %v", tc.q, got, want)
				}
			}
		})
	}
}

// TestAnalyzer_FeedIndexCreates — feed_videos использует тот же analysis-блок
// (ruEnAnalysis). Проверяем, что OpenSearch принимает его: битое правило
// синонимов роняет создание индекса, а на проде это происходит уже ПОСЛЕ
// дропа старого — то есть с пустым поиском.
func TestAnalyzer_FeedIndexCreates(t *testing.T) {
	f := setupAnalyzerFixture(t)

	feedIndex := "analyzer_test_feed_" + uuid.NewString()[:8]
	ctx := context.Background()
	if err := f.client.CreateIndex(ctx, feedIndex, search.FeedVideoMapping()); err != nil {
		t.Fatalf("create feed index: %v", err)
	}
	t.Cleanup(func() { _ = f.client.DeleteIndex(context.Background(), feedIndex) })
}

// TestAnalyzer_YoFoldingBeyondDictionary — «ё»→«е» должно работать для любого
// текста, а не только для слов из словаря синонимов: bio, имена, навыки.
// Раньше это лечилось перечислением обоих написаний в правилах — и молча
// не работало для всего, что в словарь не попало.
func TestAnalyzer_YoFoldingBeyondDictionary(t *testing.T) {
	f := setupAnalyzerFixture(t)

	const id = "66666666-6666-6666-6666-666666666666"
	f.index_(t, doc(id, "Алёна Тёркина", "", "Снимаю тёплые семейные съёмки, авторский подход", "photographer"))

	for _, q := range []string{"алена", "алёна", "теркина", "тёркина", "теплые", "тёплые"} {
		res, err := f.svc.Search(context.Background(), search.Query{Q: q, Limit: 20})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if res.Broadened || len(res.Items) != 1 || res.Items[0].UserID != id {
			t.Errorf("запрос %q не нашёл спеца через «ё»-фолдинг: total=%d broadened=%v",
				q, res.Total, res.Broadened)
		}
	}
}
