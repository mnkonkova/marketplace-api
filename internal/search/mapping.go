package search

func IndexMapping() map[string]any {
	return map[string]any{
		"settings": map[string]any{
			"number_of_shards":   1,
			"number_of_replicas": 0,
			"analysis": map[string]any{
				"analyzer": map[string]any{
					"ru_en": map[string]any{
						"type":      "custom",
						"tokenizer": "standard",
						"filter":    []string{"lowercase", "stop", "asciifolding"},
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
				"last_video_at":    map[string]any{"type": "date"},
			},
		},
	}
}
