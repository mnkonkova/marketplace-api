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
		"моушн", "motion", // категория motion + русское «моушн»
		"монтажер", "editor", // категория editor (без «ё», см. TestSynonyms_NoYo)
		"монтаж", // «монтаж» без «-ер» — юзеры пишут и так
		"сценарист", "scriptwriter",
	}
	for _, s := range must {
		if !strings.Contains(joined, s) {
			t.Errorf("synonyms should contain %q", s)
		}
	}
}

// TestSynonyms_NoYo — в словаре не должно быть «ё». Анализатор нормализует
// «ё»→«е» char_filter'ом ДО synonym-фильтра, поэтому правило с «ё» никогда
// не сматчится — это мёртвая строка, которая создаёт ложное ощущение
// покрытия (ровно так «Монтажёр» перестал находиться по «монтажер»).
func TestSynonyms_NoYo(t *testing.T) {
	for _, rule := range ruEnSynonyms {
		if strings.ContainsAny(rule, "ёЁ") {
			t.Errorf("правило содержит «ё» — оно мертво после char_filter ru_yo, пиши через «е»: %q", rule)
		}
	}
}

// TestSynonyms_NoOverBroadTerms — слишком общие слова не должны быть
// самостоятельными членами группы. Группа синонимов — класс эквивалентности:
// бареное «реклама» в группе «таргет» означало, что запрос «таргет»
// возвращает всех, у кого в bio есть слово «реклама» (например актёра).
// Такие слова допустимы только внутри фразы («таргетированная реклама»).
func TestSynonyms_NoOverBroadTerms(t *testing.T) {
	banned := map[string]bool{
		"реклама": true, "рекламы": true, "видео": true, "контент": true,
		"съемка": true, "съемки": true, "фото": true, "ролик": true,
		"ролики": true, "продвижение": true, "работа": true, "проект": true,
	}
	for _, rule := range ruEnSynonyms {
		for _, term := range strings.Split(rule, ",") {
			term = strings.ToLower(strings.TrimSpace(term))
			if banned[term] {
				t.Errorf("слишком общий термин %q как отдельный синоним в правиле %q — "+
					"он потащит в выдачу всех, у кого это слово есть в bio", term, rule)
			}
		}
	}
}

// TestSynonyms_NoTermInTwoGroups — один и тот же термин в двух группах
// молча склеивает их: «оператор» и в videographer, и в smm означал бы, что
// запрос «видеооператор» тащит сммщиков. Дубли почти всегда опечатка,
// а не намерение — если намерение, объединяй группы явно.
func TestSynonyms_NoTermInTwoGroups(t *testing.T) {
	seen := map[string]int{}
	for i, rule := range ruEnSynonyms {
		for _, term := range strings.Split(rule, ",") {
			term = strings.ToLower(strings.TrimSpace(term))
			if term == "" {
				continue
			}
			if prev, dup := seen[term]; dup {
				t.Errorf("термин %q есть и в правиле #%d, и в #%d — группы склеятся:\n  %s\n  %s",
					term, prev, i, ruEnSynonyms[prev], rule)
				continue
			}
			seen[term] = i
		}
	}
}

// TestSynonyms_AllCategoriesCovered — у каждой категории из
// specialty_categories должно быть слово в словаре, иначе запрос по
// названию профессии даст 0 хитов и уйдёт в broadening (= «показываем
// всех», из-за чего на «монтажер» показывался актёр).
func TestSynonyms_AllCategoriesCovered(t *testing.T) {
	joined := strings.ToLower(strings.Join(ruEnSynonyms, "|"))
	// code → слово, которым юзер реально ищет эту категорию.
	perCategory := map[string]string{
		"editor":         "монтажер",
		"video_director": "видеоредактор",
		"motion":         "моушн",
		"scriptwriter":   "сценарист",
		"ugc":            "ugc",
		"videographer":   "видеооператор",
		"photographer":   "фотограф",
		"actor":          "актер",
		"designer":       "дизайнер",
		"ai_creator":     "нейросети",
		"smm":            "смм",
		"blogger":        "блогер",
		"ads_seo":        "таргетолог",
		"seeding":        "посевы",
		"director":       "режиссер",
	}
	for code, word := range perCategory {
		if !strings.Contains(joined, word) {
			t.Errorf("категория %s не покрыта словарём: нет %q", code, word)
		}
	}
}

// TestMappings_HaveYoCharFilter — char_filter ru_yo должен быть подключён в
// ОБОИХ индексах. Если он есть только в specialists — поиск по /feed снова
// начнёт различать «ё» и «е».
func TestMappings_HaveYoCharFilter(t *testing.T) {
	for name, m := range map[string]map[string]any{
		"specialists": IndexMapping(),
		"feed_videos": FeedVideoMapping(),
	} {
		settings, _ := m["settings"].(map[string]any)
		analysis, _ := settings["analysis"].(map[string]any)
		charFilters, _ := analysis["char_filter"].(map[string]any)
		ruYo, ok := charFilters["ru_yo"].(map[string]any)
		if !ok {
			t.Errorf("%s: char_filter ru_yo отсутствует", name)
			continue
		}
		if ruYo["type"] != "mapping" {
			t.Errorf("%s: ru_yo type = %v, want mapping", name, ruYo["type"])
		}
		mappings, _ := ruYo["mappings"].([]string)
		if len(mappings) != 2 || mappings[0] != "ё=>е" || mappings[1] != "Ё=>Е" {
			t.Errorf("%s: ru_yo mappings = %v, want [ё=>е Ё=>Е] (заглавная нужна: "+
				"lowercase работает уже после токенизатора)", name, mappings)
		}
		analyzer, _ := analysis["analyzer"].(map[string]any)
		ruEn, _ := analyzer["ru_en"].(map[string]any)
		chain, _ := ruEn["char_filter"].([]string)
		if len(chain) != 1 || chain[0] != "ru_yo" {
			t.Errorf("%s: ru_en.char_filter = %v, want [ru_yo]", name, chain)
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
