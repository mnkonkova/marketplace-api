package search

import (
	"strings"
	"testing"
)

// TestIndexMapping_HasSynonymFilter — маппинг должен содержать
// synonym_graph фильтр с ru_en_synonyms в цепочке анализатора.
// Если этот тест падает — синонимы не будут применяться при поиске,
// и «тикток» не найдёт скилл `tiktok` (регресс #v2.1 mapping change).
func TestIndexMapping_HasSynonymFilter(t *testing.T) {
	m := IndexMapping()
	settings, _ := m["settings"].(map[string]any)
	analysis, _ := settings["analysis"].(map[string]any)
	filter, _ := analysis["filter"].(map[string]any)
	syn, ok := filter["ru_en_synonyms"].(map[string]any)
	if !ok {
		t.Fatal("ru_en_synonyms filter missing from settings.analysis.filter")
	}
	if syn["type"] != "synonym_graph" {
		t.Errorf("synonym filter type = %v, want 'synonym_graph' (нужен для мультитокенов вроде 'vk clips')", syn["type"])
	}
	// В analyzer.ru_en.filter цепочка должна заканчиваться на ru_en_synonyms.
	analyzer, _ := analysis["analyzer"].(map[string]any)
	ruEn, _ := analyzer["ru_en"].(map[string]any)
	chain, _ := ruEn["filter"].([]string)
	if len(chain) == 0 || chain[len(chain)-1] != "ru_en_synonyms" {
		t.Errorf("ru_en analyzer filter chain = %v, want ends with 'ru_en_synonyms'", chain)
	}
}

// TestFeedVideoMapping_HasSynonymFilter — симметрично для feed_videos.
// Тот же список синонимов должен быть подключён (нужно для поиска
// по title/description роликов).
func TestFeedVideoMapping_HasSynonymFilter(t *testing.T) {
	m := FeedVideoMapping()
	settings, _ := m["settings"].(map[string]any)
	analysis, _ := settings["analysis"].(map[string]any)
	filter, _ := analysis["filter"].(map[string]any)
	syn, ok := filter["ru_en_synonyms"].(map[string]any)
	if !ok {
		t.Fatal("ru_en_synonyms filter missing from feed_videos analysis")
	}
	if syn["type"] != "synonym_graph" {
		t.Errorf("feed synonym type = %v, want synonym_graph", syn["type"])
	}
}

// TestSynonyms_CriticalPairs — key pairs которые обязательно должны быть
// в списке. Если убрал случайно — тест напомнит.
func TestSynonyms_CriticalPairs(t *testing.T) {
	joined := strings.ToLower(strings.Join(ruEnSynonyms, "|"))
	must := []string{
		"tiktok", "тикток",
		"premiere", "премьер",
		"after effects", "афтер эффектс",
		"ai", "ии",
		"videographer", "видеограф",
	}
	for _, s := range must {
		if !strings.Contains(joined, s) {
			t.Errorf("synonyms should contain %q", s)
		}
	}
}

// TestIndexMapping_HasPreviewVideoFields — денормализованные preview-поля
// для рендера карточек в SearchResultsPage. Должны быть index: false —
// не участвуют в поиске / сортировке (только display).
func TestIndexMapping_HasPreviewVideoFields(t *testing.T) {
	m := IndexMapping()
	mappings, _ := m["mappings"].(map[string]any)
	props, _ := mappings["properties"].(map[string]any)
	for _, field := range []string{"preview_video_url", "preview_thumb_url", "preview_animated_url"} {
		def, ok := props[field].(map[string]any)
		if !ok {
			t.Errorf("mappings.properties.%s missing", field)
			continue
		}
		if def["type"] != "keyword" {
			t.Errorf("%s type = %v, want keyword", field, def["type"])
		}
		if def["index"] != false {
			t.Errorf("%s index = %v, want false (display-only, не для поиска)", field, def["index"])
		}
	}
}
