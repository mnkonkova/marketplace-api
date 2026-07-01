package search

// ru_en_synonyms — синонимы кириллица↔латиница для платформ, инструментов
// монтажа, жанров и категорий. Позволяют искать «тикток», «TikTok» и
// «tik-tok» с одинаковым результатом.
//
// Тип фильтра — synonym_graph (не synonym): нужен для многословных
// синонимов вроде «vk clips» или «after effects», которые токенизуются
// в несколько токенов. Simple synonym для мультитокен-запросов ломается.
//
// Список сгруппирован по типам. Обновление списка = смена маппинга =
// требует переиндексации через OPENSEARCH_REINDEX_ON_START=true.
var ruEnSynonyms = []string{
	// Платформы
	"tiktok, тикток, тик-ток",
	"reels, рилс, рилсы",
	"shorts, шортс, шортсы",
	"youtube, ютуб, ютьюб",
	"vk clips, вк клипс, вк-клипы, клипы вк",
	"telegram, телеграм, тг",
	"instagram, инстаграм, инст",
	"twitch, твич",

	// Инструменты монтажа
	"premiere, премьер, премьер про, premiere pro",
	"after effects, афтер эффектс, афтер",
	"davinci, davinci resolve, давинчи, давинчи резолв",
	"final cut, файнал кат, финал кат, final cut pro",
	"capcut, капкат, кап кат",

	// Графика
	"photoshop, фотошоп, пс",
	"figma, фигма",
	"illustrator, иллюстратор",

	// Жанры (для bio-поиска)
	"ugc, юджиси, юзер контент",
	"smm, смм, эсэмэм",

	// Категории (для поиска по свободному тексту)
	"ии, ai, ии креатор, ai креатор, генеративный, нейросети, нейросеть",
	"видеограф, videographer",
	"фотограф, photographer",
	"актёр, актер, actor",
	"дизайнер, designer",

	// Бренд площадки (люди пишут по-разному — упрощаем поиск себя же).
	"wayprod, wayprodmarket, вейпрод, вейпродмаркет",
}

func IndexMapping() map[string]any {
	return map[string]any{
		"settings": map[string]any{
			"number_of_shards":   1,
			"number_of_replicas": 0,
			"analysis": map[string]any{
				"filter": map[string]any{
					"ru_en_synonyms": map[string]any{
						"type":     "synonym_graph",
						"synonyms": ruEnSynonyms,
					},
				},
				"analyzer": map[string]any{
					"ru_en": map[string]any{
						"type":      "custom",
						"tokenizer": "standard",
						"filter":    []string{"lowercase", "stop", "asciifolding", "ru_en_synonyms"},
					},
				},
			},
		},
		"mappings": map[string]any{
			"properties": map[string]any{
				"user_id":          map[string]any{"type": "keyword"},
				"display_name":     map[string]any{"type": "text", "analyzer": "ru_en", "fields": map[string]any{"raw": map[string]any{"type": "keyword"}}},
				"bio":              map[string]any{"type": "text", "analyzer": "ru_en"},
				"avatar_url":       map[string]any{"type": "keyword", "index": false},
				"city":             map[string]any{"type": "keyword", "fields": map[string]any{"text": map[string]any{"type": "text", "analyzer": "ru_en"}}},
				"categories":       map[string]any{"type": "keyword"},
				"primary_category": map[string]any{"type": "keyword"},
				"skill_slugs":      map[string]any{"type": "keyword"},
				"skill_titles":     map[string]any{"type": "text", "analyzer": "ru_en"},
				"rate_min":         map[string]any{"type": "integer"},
				"rate_max":         map[string]any{"type": "integer"},
				"currency":         map[string]any{"type": "keyword"},
				"rating_avg":       map[string]any{"type": "float"},
				"reviews_count":    map[string]any{"type": "integer"},
				"is_published":     map[string]any{"type": "boolean"},
				"updated_at":       map[string]any{"type": "date"},
				// last_video_at — MAX(created_at) опубликованных видео спеца.
				// Денормализовано из portfolio_items, чтобы /feed мог
				// ранжировать без N запросов в PG. null если у спеца нет видео.
				"last_video_at": map[string]any{"type": "date"},
				// preview_video_url + preview_thumb_url — денормализовано из
				// последнего опубликованного видео спеца (для карточки в
				// SearchResultsPage). index: false — только display, не
				// участвует в поиске/сортировке.
				"preview_video_url":  map[string]any{"type": "keyword", "index": false},
				"preview_thumb_url":  map[string]any{"type": "keyword", "index": false},
				"preview_animated_url": map[string]any{"type": "keyword", "index": false},
				// production_name — название студии или пусто (фрилансер / не выбрал).
				// Денормализовано из productions, чтобы карточка спеца в поиске
				// рендерилась без JOIN. Деактивированные studios резолвятся в "".
				"production_name": map[string]any{"type": "text", "analyzer": "ru_en", "fields": map[string]any{"raw": map[string]any{"type": "keyword"}}},
				"is_freelance":    map[string]any{"type": "boolean"},
			},
		},
	}
}
