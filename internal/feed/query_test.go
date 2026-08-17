package feed

import (
	"testing"

	"github.com/google/uuid"
)

// sortFields — имена полей сортировки из тела запроса, по порядку.
func sortFields(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["sort"].([]any)
	if !ok {
		t.Fatalf("в запросе нет sort: %#v", body["sort"])
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok || len(m) != 1 {
			t.Fatalf("неожиданный элемент sort: %#v", item)
		}
		for field := range m {
			out = append(out, field)
		}
	}
	return out
}

func TestBuildFeedQuery_DiscoverySort(t *testing.T) {
	body := buildFeedQuery(Query{}, cursorPayload{})
	got := sortFields(t, body)
	want := []string{"rating_avg", "video_created_at", "video_id"}
	if len(got) != len(want) {
		t.Fatalf("sort = %v, ожидали %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sort[%d] = %q, ожидали %q", i, got[i], want[i])
		}
	}
}

// Лента одного специалиста открывается с его страницы — порядок обязан
// совпадать с тем, что там видно: сначала закреплённая работа, дальше
// порядок, который выставил сам специалист. Иначе после промо свайп
// уводит в произвольную последовательность (до этого фикса лента шла по
// video_created_at, то есть ровно наоборот к sort_order).
func TestBuildFeedQuery_SingleSpecialistSortsByFeaturedThenOrder(t *testing.T) {
	body := buildFeedQuery(Query{UserIDs: []uuid.UUID{uuid.New()}}, cursorPayload{})
	got := sortFields(t, body)
	want := []string{"is_featured", "sort_order", "video_created_at", "video_id"}
	if len(got) != len(want) {
		t.Fatalf("sort = %v, ожидали %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sort[%d] = %q, ожидали %q", i, got[i], want[i])
		}
	}
}

func TestBuildFeedQuery_SingleSpecialistFeaturedFirst(t *testing.T) {
	body := buildFeedQuery(Query{UserIDs: []uuid.UUID{uuid.New()}}, cursorPayload{})
	first := body["sort"].([]any)[0].(map[string]any)
	dir, ok := first["is_featured"].(map[string]any)
	if !ok {
		t.Fatalf("первым полем сортировки ожидали is_featured, получили %#v", first)
	}
	if dir["order"] != "desc" {
		t.Errorf("is_featured order = %v, ожидали desc (true впереди)", dir["order"])
	}
}

// Несколько спецов — это уже подборка, а не портфолио одного человека:
// сортировать по чужому sort_order бессмысленно, остаётся дискавери-порядок.
func TestBuildFeedQuery_MultipleSpecialistsKeepDiscoverySort(t *testing.T) {
	body := buildFeedQuery(Query{UserIDs: []uuid.UUID{uuid.New(), uuid.New()}}, cursorPayload{})
	if got := sortFields(t, body)[0]; got != "rating_avg" {
		t.Errorf("первое поле сортировки = %q, ожидали rating_avg", got)
	}
}

// Тай-брейк по video_id обязателен: без него search_after на равных
// sort_order отдаёт недетерминированный порядок и страницы дублируются.
func TestBuildFeedQuery_TieBreakerAlwaysLast(t *testing.T) {
	for name, q := range map[string]Query{
		"дискавери": {},
		"один спец": {UserIDs: []uuid.UUID{uuid.New()}},
		"несколько": {UserIDs: []uuid.UUID{uuid.New(), uuid.New()}},
	} {
		fields := sortFields(t, buildFeedQuery(q, cursorPayload{}))
		if last := fields[len(fields)-1]; last != "video_id" {
			t.Errorf("%s: последним полем сортировки ожидали video_id, получили %q", name, last)
		}
	}
}

// Курсор из одной ленты, присланный в другую, отличается арностью
// search_after (3 значения против 4). Без сброса ES отвечает ошибкой, а
// юзер получает 502 на ровном месте.
func TestBuildFeedQuery_DropsCursorWithMismatchedArity(t *testing.T) {
	stale := cursorPayload{SearchAfter: []any{4.2, "2026-01-01", "vid"}} // 3 значения, дискавери
	body := buildFeedQuery(Query{UserIDs: []uuid.UUID{uuid.New()}}, stale)
	if _, ok := body["search_after"]; ok {
		t.Error("курсор чужой арности должен игнорироваться, а не уезжать в ES")
	}
}

func TestBuildFeedQuery_KeepsCursorWithMatchingArity(t *testing.T) {
	ok4 := cursorPayload{SearchAfter: []any{true, 0, "2026-01-01", "vid"}} // 4 значения
	body := buildFeedQuery(Query{UserIDs: []uuid.UUID{uuid.New()}}, ok4)
	if _, ok := body["search_after"]; !ok {
		t.Error("валидный курсор должен доезжать до ES")
	}
}
