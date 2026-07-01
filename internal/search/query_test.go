package search

import (
	"reflect"
	"testing"
)

// TestBuildQuery_MultiMatchFields — регресс v2.1 фикса: в multi_match
// должно быть category_titles^2. Без него запрос «моушн-дизайнер» не
// матчится на спеца с primary_category=motion — user получает 0.
func TestBuildQuery_MultiMatchFields(t *testing.T) {
	body := buildQuery(Query{Q: "тест"}, queryOpts{})
	got := mustGet[[]string](t, body,
		"query", "bool", "must", "multi_match", "fields")
	want := []string{"display_name^3", "category_titles^2", "bio", "skill_titles", "city.text"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi_match.fields = %v\nwant %v", got, want)
	}
}

// TestBuildQuery_EmptyQMatchAll — без текстового запроса используем match_all
// (иначе OS вернёт 0 при пустой строке).
func TestBuildQuery_EmptyQMatchAll(t *testing.T) {
	body := buildQuery(Query{}, queryOpts{})
	must := mustGet[map[string]any](t, body, "query", "bool", "must")
	if _, ok := must["match_all"]; !ok {
		t.Errorf("empty Q → must = %v, want match_all", must)
	}
}

// TestBuildQuery_CategoryInPostFilter — категории идут через post_filter,
// не через bool.filter. Иначе агрегация по категориям схлопнется до
// одного bucket'а и facet-панель на фронте станет бесполезной.
func TestBuildQuery_CategoryInPostFilter(t *testing.T) {
	body := buildQuery(Query{Categories: []string{"director"}}, queryOpts{})
	pf, ok := body["post_filter"]
	if !ok {
		t.Fatal("post_filter отсутствует при заданной категории")
	}
	terms := mustGet[map[string]any](t, pf.(map[string]any), "terms")
	got, _ := terms["categories"].([]string)
	if !reflect.DeepEqual(got, []string{"director"}) {
		t.Errorf("post_filter.terms.categories = %v, want [director]", got)
	}
	// И категория НЕ должна дублироваться в bool.filter.
	filters := mustGet[[]any](t, body, "query", "bool", "filter")
	for _, f := range filters {
		if m, _ := f.(map[string]any); m != nil {
			if terms, ok := m["terms"].(map[string]any); ok {
				if _, hasCat := terms["categories"]; hasCat {
					t.Errorf("categories попали в bool.filter — сломается агрегация: %v", filters)
				}
			}
		}
	}
}

// TestBuildQuery_HardFiltersSkillsAlwaysApplied — скилы это hard-фильтр,
// в bool.filter. Не должны релаксироваться (в отличие от city/rate).
func TestBuildQuery_HardFiltersSkillsAlwaysApplied(t *testing.T) {
	body := buildQuery(Query{SkillSlugs: []string{"premiere", "aftereffects"}}, queryOpts{})
	filters := mustGet[[]any](t, body, "query", "bool", "filter")
	found := false
	for _, f := range filters {
		m, _ := f.(map[string]any)
		terms, _ := m["terms"].(map[string]any)
		got, _ := terms["skill_slugs"].([]string)
		if reflect.DeepEqual(got, []string{"premiere", "aftereffects"}) {
			found = true
		}
	}
	if !found {
		t.Errorf("skill_slugs terms-фильтр не найден в bool.filter: %v", filters)
	}
}

// TestBuildQuery_IsPublishedAlwaysFilter — is_published=true всегда есть,
// иначе в поиск попадут неопубликованные / забаненные спецы.
func TestBuildQuery_IsPublishedAlwaysFilter(t *testing.T) {
	body := buildQuery(Query{}, queryOpts{})
	filters := mustGet[[]any](t, body, "query", "bool", "filter")
	if len(filters) == 0 {
		t.Fatal("нет фильтров, is_published должен быть первым")
	}
	m, _ := filters[0].(map[string]any)
	term, _ := m["term"].(map[string]any)
	if term["is_published"] != true {
		t.Errorf("первый фильтр не is_published=true: %v", filters[0])
	}
}

// TestBuildQuery_ExcludeIDsMustNot — excludeIDs идут в must_not.
// Используется релакс-запросом чтобы не дублировать основные Items.
func TestBuildQuery_ExcludeIDsMustNot(t *testing.T) {
	body := buildQuery(Query{}, queryOpts{excludeIDs: []string{"u1", "u2"}})
	mn, ok := mustGet[map[string]any](t, body, "query", "bool")["must_not"].([]any)
	if !ok || len(mn) == 0 {
		t.Fatal("must_not отсутствует при excludeIDs")
	}
	m, _ := mn[0].(map[string]any)
	terms, _ := m["terms"].(map[string]any)
	got, _ := terms["user_id"].([]string)
	if !reflect.DeepEqual(got, []string{"u1", "u2"}) {
		t.Errorf("must_not.terms.user_id = %v, want [u1 u2]", got)
	}
}

// TestBuildQuery_SkipFacets — при skipFacets=true агрегаций нет.
// Релакс-запрос ставит этот флаг: агрегация нужна только основного результата.
func TestBuildQuery_SkipFacets(t *testing.T) {
	full := buildQuery(Query{}, queryOpts{})
	if _, ok := full["aggs"]; !ok {
		t.Error("без skipFacets aggs должны быть")
	}
	skipped := buildQuery(Query{}, queryOpts{skipFacets: true})
	if _, ok := skipped["aggs"]; ok {
		t.Error("skipFacets=true, но aggs присутствуют")
	}
}

// TestSoftFiltersInQuery_TriggerRelaxation — soft-фильтры (city, rate)
// возвращаются, hard-фильтры (skills, categories) — нет. Именно от этого
// зависит триггер релакс-запроса в Service.Search.
func TestSoftFiltersInQuery(t *testing.T) {
	rateMin := 1000
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"empty", Query{}, nil},
		{"city only", Query{City: "msk"}, []string{"city"}},
		{"rate_min", Query{RateMin: &rateMin}, []string{"rate"}},
		{"category is hard", Query{Categories: []string{"director"}}, nil},
		{"skills are hard", Query{SkillSlugs: []string{"premiere"}}, nil},
		{"city + rate", Query{City: "msk", RateMin: &rateMin}, []string{"city", "rate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := softFiltersInQuery(tc.q)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("softFiltersInQuery = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildQuery_RateRangeHalfOpen — RateMin юзера значит «спец берёт
// не меньше X» → matches спецов у которых rate_max >= X. Симметрично
// для RateMax → rate_min <= X. Проверка что не перепутано направление.
func TestBuildQuery_RateRangeHalfOpen(t *testing.T) {
	minVal, maxVal := 1000, 5000
	body := buildQuery(Query{RateMin: &minVal, RateMax: &maxVal}, queryOpts{})
	filters := mustGet[[]any](t, body, "query", "bool", "filter")
	var gotMin, gotMax any
	for _, f := range filters {
		m, _ := f.(map[string]any)
		r, _ := m["range"].(map[string]any)
		if rm, ok := r["rate_max"].(map[string]any); ok {
			gotMin = rm["gte"]
		}
		if rm, ok := r["rate_min"].(map[string]any); ok {
			gotMax = rm["lte"]
		}
	}
	if gotMin != 1000 {
		t.Errorf("rate_max.gte = %v, want 1000 (RateMin юзера → rate_max спеца >= X)", gotMin)
	}
	if gotMax != 5000 {
		t.Errorf("rate_min.lte = %v, want 5000", gotMax)
	}
}

// TestBuildQuery_SortByScoreThenRating — сортировка _score → rating →
// reviews_count. Регрессия: при переставлении rating выше _score
// спецы-новички без отзывов вылетали в конец даже при точном матче.
func TestBuildQuery_SortByScoreThenRating(t *testing.T) {
	body := buildQuery(Query{Q: "x"}, queryOpts{})
	sort, _ := body["sort"].([]any)
	if len(sort) < 3 {
		t.Fatalf("sort = %v, want ≥ 3 элемента", sort)
	}
	if sort[0] != "_score" {
		t.Errorf("sort[0] = %v, want _score", sort[0])
	}
	m1, _ := sort[1].(map[string]any)
	if _, ok := m1["rating_avg"]; !ok {
		t.Errorf("sort[1] must be rating_avg, got %v", sort[1])
	}
}

// mustGet — walk по nested map[string]any. Fatal если путь не сходится.
// Не используем reflect чтобы получать типизированные значения.
func mustGet[T any](t *testing.T, root map[string]any, path ...string) T {
	t.Helper()
	var cur any = root
	for i, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: элемент %d не map (тип %T)", path, i, cur)
		}
		cur, ok = m[key]
		if !ok {
			t.Fatalf("path %v: ключа %q нет; keys=%v", path, key, keysOf(m))
		}
	}
	v, ok := cur.(T)
	if !ok {
		var zero T
		t.Fatalf("path %v: тип %T, want %T", path, cur, zero)
	}
	return v
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
