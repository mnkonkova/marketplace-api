package search_test

import (
	"encoding/json"
	"testing"

	"marketpclce/internal/search"
)

func TestParseCategoryAggsEmpty(t *testing.T) {
	got := search.ParseCategoryAggs(nil)
	if len(got) != 0 {
		t.Errorf("nil вход → пустой выход, got %v", got)
	}
	got = search.ParseCategoryAggs(json.RawMessage(`{}`))
	if len(got) != 0 {
		t.Errorf("пустой aggs → пустой выход, got %v", got)
	}
}

func TestParseCategoryAggsHappy(t *testing.T) {
	raw := json.RawMessage(`{
		"categories": {
			"buckets": [
				{"key": "editor", "doc_count": 12},
				{"key": "motion", "doc_count": 7},
				{"key": "smm", "doc_count": 3}
			]
		}
	}`)
	got := search.ParseCategoryAggs(raw)
	if len(got) != 3 {
		t.Fatalf("ожидаем 3 категории, got %d (%v)", len(got), got)
	}
	type pair struct {
		Code  string
		Count int
	}
	want := []pair{{"editor", 12}, {"motion", 7}, {"smm", 3}}
	for i, w := range want {
		if got[i].Code != w.Code || got[i].Count != w.Count {
			t.Errorf("index %d: got %+v, want {%s %d}", i, got[i], w.Code, w.Count)
		}
	}
}

// Malformed JSON не должен паниковать — вернуть пусто. Это важно, потому
// что хэндлер не проверяет ошибку парсинга агрегаций отдельно.
func TestParseCategoryAggsMalformed(t *testing.T) {
	got := search.ParseCategoryAggs(json.RawMessage(`{ broken }`))
	if len(got) != 0 {
		t.Errorf("malformed JSON → пустой выход, got %v", got)
	}
}
