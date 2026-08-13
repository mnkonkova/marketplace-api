package search

// FeedVideoMapping — индекс под /feed. Юнит = одно видео; поля специалиста
// денормализованы, чтобы лента не делала второй round-trip за карточкой
// спеца. Обновление одного видео = один upsert; обновление профиля = пакет
// upsert'ов по всем видео спеца (см. feed_indexer.go ReconcileVideos).
//
// Цена денормализации: лишний storage и работа индексеру при изменении
// rating_avg/display_name/avatar спеца — все его видео переписываются. Для
// MVP-объёма (≤20 видео на спеца, см. portfolioMaxVideosPerUser) это
// приемлемо.
func FeedVideoMapping() map[string]any {
	return map[string]any{
		"settings": map[string]any{
			"number_of_shards":   1,
			"number_of_replicas": 0,
			// Тот же анализатор что и в основном IndexMapping
			// (см. mapping.go::ruEnAnalysis) — один источник истины, чтобы
			// кириллица↔латиница и «ё»→«е» работали и в feed-поиске тоже
			// (feed multi_match ищет по title/description/bio).
			"analysis": ruEnAnalysis(),
		},
		"mappings": map[string]any{
			"properties": map[string]any{
				// айтем: kind='video' | 'image'. Имена полей video_* legacy
				// сохранены (индекс называется feed_videos), семантически —
				// item_id / item_url. Для kind='image' video_url пустой,
				// карусель живёт в images[].
				"kind":             map[string]any{"type": "keyword"},
				"video_id":         map[string]any{"type": "keyword"},
				"video_url":        map[string]any{"type": "keyword", "index": false},
				"thumb_url":        map[string]any{"type": "keyword", "index": false},
				"images": map[string]any{
					"type": "object",
					"enabled": false, // не индексируем, только хранение
				},
				"title":            map[string]any{"type": "text", "analyzer": "ru_en"},
				"description":      map[string]any{"type": "text", "analyzer": "ru_en"},
				"duration_sec":     map[string]any{"type": "integer"},
				"aspect":           map[string]any{"type": "keyword"},
				"video_created_at": map[string]any{"type": "date"},
				"category_codes":   map[string]any{"type": "keyword"},

				// денормализованный специалист (для фильтрации/отображения)
				"user_id":          map[string]any{"type": "keyword"},
				"display_name":     map[string]any{"type": "text", "analyzer": "ru_en", "fields": map[string]any{"raw": map[string]any{"type": "keyword"}}},
				"avatar_url":       map[string]any{"type": "keyword", "index": false},
				"bio":              map[string]any{"type": "text", "analyzer": "ru_en"},
				"city":             map[string]any{"type": "keyword", "fields": map[string]any{"text": map[string]any{"type": "text", "analyzer": "ru_en"}}},
				"rate_min":         map[string]any{"type": "integer"},
				"rate_max":         map[string]any{"type": "integer"},
				"currency":         map[string]any{"type": "keyword"},
				"categories":       map[string]any{"type": "keyword"},
				"primary_category": map[string]any{"type": "keyword"},
				"rating_avg":       map[string]any{"type": "float"},
				"reviews_count":    map[string]any{"type": "integer"},
				"is_published":     map[string]any{"type": "boolean"},
				"production_name":  map[string]any{"type": "text", "analyzer": "ru_en", "fields": map[string]any{"raw": map[string]any{"type": "keyword"}}},
				"is_freelance":     map[string]any{"type": "boolean"},
			},
		},
	}
}
