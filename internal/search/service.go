package search

import (
	"context"
	"encoding/json"
	"fmt"

	"marketpclce/internal/platform/es"
)

type Service struct {
	es    *es.Client
	index string
}

func NewService(esClient *es.Client, index string) *Service {
	return &Service{es: esClient, index: index}
}

type Query struct {
	Q          string
	Categories []string
	SkillSlugs []string
	City       string
	RateMin    *int
	RateMax    *int
	Limit      int
	Offset     int
}

type CategoryCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

type Facets struct {
	Categories []CategoryCount `json:"categories"`
}

type Result struct {
	Total     int        `json:"total"`
	Items     []IndexDoc `json:"items"`
	Similar   []IndexDoc `json:"similar,omitempty"`
	Relaxed   []string   `json:"relaxed,omitempty"`
	Broadened bool       `json:"broadened,omitempty"`
	Facets    *Facets    `json:"facets,omitempty"`
}

// similarThreshold — если строгий проход вернул меньше, добираем «похожих» с ослабленными soft-фильтрами.
const similarThreshold = 5

func (s *Service) Search(ctx context.Context, q Query) (Result, error) {
	if q.Limit <= 0 || q.Limit > 50 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	out, err := s.runQuery(ctx, q, queryOpts{broadened: false})
	if err != nil {
		return Result{}, err
	}

	// Старый бродинг для пустой текстовой выдачи — оставляем для совместимости с summarize-сценарием.
	if out.Total == 0 && q.Q != "" {
		broadened := q
		broadened.Q = ""
		out2, err := s.runQuery(ctx, broadened, queryOpts{broadened: true})
		if err != nil {
			return Result{}, err
		}
		return out2, nil
	}

	// Ленивая релаксация: если строгий результат скудный и есть soft-фильтры — добираем «похожих».
	// Триггерим по Total (а не len(Items)) — иначе при маленьком Limit релаксация бьёт зря.
	if q.Offset == 0 && out.Total < similarThreshold {
		relaxed := softFiltersInQuery(q)
		if len(relaxed) > 0 {
			excludeIDs := make([]string, 0, len(out.Items))
			for _, d := range out.Items {
				excludeIDs = append(excludeIDs, d.UserID)
			}
			soft := q
			soft.City = ""
			soft.RateMin = nil
			soft.RateMax = nil
			soft.Limit = similarThreshold * 3
			soft.Offset = 0
			out2, err := s.runQuery(ctx, soft, queryOpts{excludeIDs: excludeIDs, skipFacets: true})
			if err == nil && len(out2.Items) > 0 {
				out.Similar = out2.Items
				out.Relaxed = relaxed
			}
		}
	}

	return out, nil
}

// softFiltersInQuery возвращает имена soft-фильтров, которые присутствуют в запросе и могут быть ослаблены.
func softFiltersInQuery(q Query) []string {
	var out []string
	if q.City != "" {
		out = append(out, "city")
	}
	if q.RateMin != nil || q.RateMax != nil {
		out = append(out, "rate")
	}
	return out
}

// CategoryStats возвращает счётчики опубликованных спецов по всем категориям
// (источник истины — ES, где работает is_published-фильтр и outbox-индексация).
func (s *Service) CategoryStats(ctx context.Context) ([]CategoryCount, error) {
	body := map[string]any{
		"size": 0,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"is_published": true}},
				},
			},
		},
		"aggs": map[string]any{
			"categories": map[string]any{
				"terms": map[string]any{"field": "categories", "size": 50},
			},
		},
	}
	resp, err := s.es.Search(ctx, s.index, body)
	if err != nil {
		return nil, fmt.Errorf("es category stats: %w", err)
	}
	return parseCategoryAggs(resp.Aggregations), nil
}

// LoadDocsByIDs — батч-фетч опубликованных спецов из ES по списку user_id.
// Используется в /feed?ids=... — когда фронт уже знает кого хочет в ленте
// (например, после /search или /search/summarize) и не нужно прогонять
// текстовый запрос. Возвращает только опубликованных спецов (как и Search):
// если кто-то снят с публикации между шагами «поиск → лента», он не покажется.
// Порядок документов в ответе — как ES вернул (по релевантности _score внутри
// terms-фильтра); смысловой ranking делает вызывающий код.
func (s *Service) LoadDocsByIDs(ctx context.Context, ids []string) ([]IndexDoc, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	body := map[string]any{
		"from": 0,
		"size": len(ids),
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"is_published": true}},
					map[string]any{"terms": map[string]any{"user_id": ids}},
				},
			},
		},
	}
	resp, err := s.es.Search(ctx, s.index, body)
	if err != nil {
		return nil, fmt.Errorf("es load by ids: %w", err)
	}
	out := make([]IndexDoc, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		var doc IndexDoc
		if err := json.Unmarshal(h.Source, &doc); err != nil {
			return nil, fmt.Errorf("decode hit: %w", err)
		}
		out = append(out, doc)
	}
	return out, nil
}

// CountByCategory считает опубликованных спецов в одной категории.
func (s *Service) CountByCategory(ctx context.Context, code string) (int, error) {
	if code == "" {
		return 0, nil
	}
	body := map[string]any{
		"size": 0,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"is_published": true}},
					map[string]any{"term": map[string]any{"categories": code}},
				},
			},
		},
	}
	resp, err := s.es.Search(ctx, s.index, body)
	if err != nil {
		return 0, fmt.Errorf("es count by category: %w", err)
	}
	return resp.Hits.Total.Value, nil
}

type queryOpts struct {
	broadened  bool
	excludeIDs []string
	skipFacets bool
}

func (s *Service) runQuery(ctx context.Context, q Query, opts queryOpts) (Result, error) {
	body := buildQuery(q, opts)
	resp, err := s.es.Search(ctx, s.index, body)
	if err != nil {
		return Result{}, fmt.Errorf("es search: %w", err)
	}
	out := Result{
		Total:     resp.Hits.Total.Value,
		Items:     make([]IndexDoc, 0, len(resp.Hits.Hits)),
		Broadened: opts.broadened,
	}
	for _, h := range resp.Hits.Hits {
		var doc IndexDoc
		if err := json.Unmarshal(h.Source, &doc); err != nil {
			return Result{}, fmt.Errorf("decode hit: %w", err)
		}
		out.Items = append(out.Items, doc)
	}
	if !opts.skipFacets {
		if cats := parseCategoryAggs(resp.Aggregations); len(cats) > 0 {
			out.Facets = &Facets{Categories: cats}
		}
	}
	return out, nil
}

// parseCategoryAggs — общая распаковка bucket'ов из `aggregations.categories.buckets`.
func parseCategoryAggs(raw json.RawMessage) []CategoryCount {
	if len(raw) == 0 {
		return nil
	}
	var aggs struct {
		Categories struct {
			Buckets []struct {
				Key      string `json:"key"`
				DocCount int    `json:"doc_count"`
			} `json:"buckets"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(raw, &aggs); err != nil {
		return nil
	}
	out := make([]CategoryCount, 0, len(aggs.Categories.Buckets))
	for _, b := range aggs.Categories.Buckets {
		out = append(out, CategoryCount{Code: b.Key, Count: b.DocCount})
	}
	return out
}

func buildQuery(q Query, opts queryOpts) map[string]any {
	// hard-фильтры: is_published, skills (роль/специализация); soft-фильтры (city, rate) добавляются ниже,
	// если их не отключили в opts (релаксация).
	filters := []any{
		map[string]any{"term": map[string]any{"is_published": true}},
	}
	if len(q.SkillSlugs) > 0 {
		filters = append(filters, map[string]any{
			"terms": map[string]any{"skill_slugs": q.SkillSlugs},
		})
	}
	if q.City != "" {
		filters = append(filters, map[string]any{
			"term": map[string]any{"city": q.City},
		})
	}
	if q.RateMin != nil {
		filters = append(filters, map[string]any{
			"range": map[string]any{"rate_max": map[string]any{"gte": *q.RateMin}},
		})
	}
	if q.RateMax != nil {
		filters = append(filters, map[string]any{
			"range": map[string]any{"rate_min": map[string]any{"lte": *q.RateMax}},
		})
	}

	var must any
	if q.Q != "" {
		must = map[string]any{
			"multi_match": map[string]any{
				"query":                q.Q,
				"fields":               []string{"display_name^3", "bio", "skill_titles", "city.text"},
				"operator":             "or",
				"type":                 "best_fields",
				"minimum_should_match": "60%",
			},
		}
	} else {
		must = map[string]any{"match_all": map[string]any{}}
	}

	sort := []any{
		"_score",
		map[string]any{"rating_avg": map[string]any{"order": "desc"}},
		map[string]any{"reviews_count": map[string]any{"order": "desc"}},
	}

	boolQ := map[string]any{
		"must":   must,
		"filter": filters,
	}
	if len(opts.excludeIDs) > 0 {
		boolQ["must_not"] = []any{
			map[string]any{"terms": map[string]any{"user_id": opts.excludeIDs}},
		}
	}

	body := map[string]any{
		"from":  q.Offset,
		"size":  q.Limit,
		"query": map[string]any{"bool": boolQ},
		"sort":  sort,
	}

	if !opts.skipFacets {
		body["aggs"] = map[string]any{
			"categories": map[string]any{
				"terms": map[string]any{"field": "categories", "size": 50},
			},
		}
	}

	// Категория — в post_filter, чтобы агрегация по категориям оставалась информативной
	// (иначе при выбранной категории facets схлопнулись бы до одного bucket'а).
	if len(q.Categories) > 0 {
		body["post_filter"] = map[string]any{
			"terms": map[string]any{"categories": q.Categories},
		}
	}

	return body
}
