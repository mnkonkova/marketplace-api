package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/platform/es"
	"marketpclce/internal/search"
)

// ErrBadCursor — курсор невалиден (битый base64 / JSON). Хендлер мапит
// в 400, не 502: это client-error (stale localStorage, ручные правки
// URL, скраперы). Раньше ловилось общим err→502 и раздувало HighErrorRate
// + ESErrorRateHigh на несуществующие ES-проблемы.
var ErrBadCursor = errors.New("bad cursor")

// pageSize — сколько видео тянем из ES за один запрос. Это потолок одной
// страницы фронту. С учётом diversity-interleave фактическое разнообразие
// спецов будет меньше pageSize (зависит от их количества среди топа).
const pageSize = 50

// seenVideosCap — потолок размера seen-сета в курсоре (видео уже отдавали,
// исключаем через must_not.terms.video_id). FIFO-обрезка.
//
// Размер выбран так:
//   - pageSize=50, значит cap=500 ≈ 10 страниц глубокого скролла без дублей.
//     После 10 страниц юзер в дискавери-ленте уже видел львиную долю
//     корпуса, повтор «лидеров» из первых страниц ок.
//   - В base64 JSON ≈ 22 KB. Хорошо помещается в URL и заголовки (большинство
//     серверов/CDN держат до 32 KB без вопросов; nginx/caddy — 8 KB по
//     дефолту, но это header_size, не query_string).
//   - Запас на рост корпуса: даже если завтра станет 5000 видео в индексе,
//     500-блеклист всё ещё закрывает разумный сеанс.
//
// При корпусе 10k+ видео и стабильных >20 страницах глубины — пересматривать
// в сторону truncated hash или bloom.
const seenVideosCap = 500

type Service struct {
	es    *es.Client
	index string
	cache *Cache
}

func NewService(esClient *es.Client, index string) *Service {
	return &Service{es: esClient, index: index}
}

// WithCache подключает Redis-кэш страниц. nil-safe.
func (s *Service) WithCache(c *Cache) *Service {
	s.cache = c
	return s
}

// cursorPayload — состояние курсора v2 (Stage 2).
//
// SearchAfter — массив значений sort'а последнего хита предыдущей страницы.
// Передаётся в ES как `search_after` — нативный курсор Elastic, стабильный
// под параллельные правки индекса.
//
// SeenVideos — video_id'шки, уже отданные в прошлых страницах. Идут в
// `must_not.terms.video_id`, чтобы при пере-ранжировании (если в индекс
// добавили новые видео между запросами) уже виденные не пришли заново.
// FIFO-обрезка до seenVideosCap.
//
// Поле Seen из v1 (user_id'шки) больше не нужно: юнит ленты теперь видео,
// дедуп идёт по video_id. Старые v1-курсоры от старого фронта раскодируются
// в SeenVideos=nil — это даст «начни с начала», без поломки.
type cursorPayload struct {
	SearchAfter []any    `json:"sa,omitempty"`
	SeenVideos  []string `json:"sv,omitempty"`
}

func (s *Service) Feed(ctx context.Context, q Query) (Result, error) {
	// Жёсткий ids-фильтр перекрывает любые soft-фильтры: юзер уже выбрал
	// кого смотреть на предыдущем шаге, не нужно ещё раз срезать по категории.
	if len(q.UserIDs) > 0 {
		q.Categories = nil
		q.SkillSlugs = nil
		q.City = ""
		q.Q = ""
	}

	// Кэш-чек до тяжёлых запросов в ES. Redis лимитируем 200мс — если
	// он залипнет (network, сервер тормозит), падаем в ES без задержки
	// вместо того чтобы утопить весь /feed в 10-сек ожидании.
	{
		cacheCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		if cached, ok := s.cache.Get(cacheCtx, q); ok {
			cancel()
			return cached, nil
		}
		cancel()
	}

	var cursor cursorPayload
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return Result{}, fmt.Errorf("%w: decode: %v", ErrBadCursor, err)
		}
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return Result{}, fmt.Errorf("%w: unmarshal: %v", ErrBadCursor, err)
		}
	}

	body := buildFeedQuery(q, cursor)
	resp, err := s.es.Search(ctx, s.index, body)
	if err != nil {
		return Result{}, fmt.Errorf("es search: %w", err)
	}

	docs := make([]search.FeedVideoDoc, 0, len(resp.Hits.Hits))
	var lastSort []any
	for _, h := range resp.Hits.Hits {
		var d search.FeedVideoDoc
		if err := json.Unmarshal(h.Source, &d); err != nil {
			return Result{}, fmt.Errorf("decode hit: %w", err)
		}
		docs = append(docs, d)
		if len(h.Sort) > 0 {
			lastSort = h.Sort
		}
	}

	if len(docs) == 0 {
		out := Result{Items: []Item{}, Total: resp.Hits.Total.Value}
		s.cache.Set(ctx, q, out)
		return out, nil
	}

	// Diversity: round-robin по user_id, чтобы 5 видео одного спеца подряд
	// не валились. Внутри юзера сохраняется ES-порядок (score DESC).
	interleaved := InterleaveByUser(docs)

	// video_idx / video_total — позиция и количество видео этого спеца
	// в текущей странице. Используется фронтом для overlay "N/M".
	totals := make(map[string]int, len(interleaved))
	for _, d := range interleaved {
		totals[d.UserID]++
	}
	idxs := make(map[string]int, len(interleaved))

	items := make([]Item, 0, len(interleaved))
	for _, d := range interleaved {
		idx := idxs[d.UserID]
		idxs[d.UserID]++
		kind := d.Kind
		if kind == "" {
			kind = "video" // back-compat для доков без поля (старые индексы)
		}
		items = append(items, Item{
			Kind:       kind,
			Video:      videoFromDoc(d),
			Images:     imagesFromDoc(d),
			Specialist: specFromFeedDoc(d),
			VideoIdx:   idx,
			VideoTotal: totals[d.UserID],
		})
	}

	out := Result{Items: items, Total: resp.Hits.Total.Value}

	// Курсор есть смысл только если страница забилась полностью — иначе
	// дальше нечего показывать.
	if len(docs) == pageSize && lastSort != nil {
		newSeen := append([]string(nil), cursor.SeenVideos...)
		for _, d := range docs {
			newSeen = append(newSeen, d.VideoID)
		}
		if over := len(newSeen) - seenVideosCap; over > 0 {
			newSeen = newSeen[over:]
		}
		next := cursorPayload{SearchAfter: lastSort, SeenVideos: newSeen}
		raw, _ := json.Marshal(next)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}

	s.cache.Set(ctx, q, out)
	return out, nil
}

// buildFeedQuery — тело запроса в feed_videos.
//
// Структура bool:
//
//	must:     full-text по q (или match_all если q пуст)
//	filter:   is_published, terms.user_id (для ids-флоу) | terms.categories | term.city
//	must_not: terms.video_id (seen-set из курсора)
//
// Sort: rating_avg DESC, video_created_at DESC, video_id ASC.
// Последнее поле — стабильный tiebreak; ES требует уникальное последнее
// поле в sort, чтобы search_after работал детерминированно.
func buildFeedQuery(q Query, cursor cursorPayload) map[string]any {
	filters := []any{
		map[string]any{"term": map[string]any{"is_published": true}},
	}
	if len(q.UserIDs) > 0 {
		ids := make([]string, len(q.UserIDs))
		for i, id := range q.UserIDs {
			ids[i] = id.String()
		}
		filters = append(filters, map[string]any{
			"terms": map[string]any{"user_id": ids},
		})
	} else {
		if len(q.Categories) > 0 {
			filters = append(filters, map[string]any{
				"terms": map[string]any{"category_codes": q.Categories},
			})
		}
		if q.City != "" {
			filters = append(filters, map[string]any{
				"term": map[string]any{"city": q.City},
			})
		}
	}

	var must any
	// Skills из AI-флоу clarify подмешиваем в Q как текст — у нас в
	// feed_videos нет индексированных skill_slugs (это решение схемы, см.
	// FeedVideoMapping). Фронт хочет жёсткого skill-фильтра — пусть идёт
	// через /search → /feed?ids=...
	searchQ := strings.TrimSpace(q.Q)
	if len(q.SkillSlugs) > 0 {
		extra := strings.Join(q.SkillSlugs, " ")
		if searchQ == "" {
			searchQ = extra
		} else {
			searchQ = searchQ + " " + extra
		}
	}
	if searchQ != "" {
		must = map[string]any{
			"multi_match": map[string]any{
				"query":                searchQ,
				"fields":               []string{"display_name^3", "title^2", "bio", "description", "city.text"},
				"operator":             "or",
				"type":                 "best_fields",
				"minimum_should_match": "60%",
			},
		}
	} else {
		must = map[string]any{"match_all": map[string]any{}}
	}

	boolQ := map[string]any{
		"must":   must,
		"filter": filters,
	}
	if len(cursor.SeenVideos) > 0 {
		boolQ["must_not"] = []any{
			map[string]any{"terms": map[string]any{"video_id": cursor.SeenVideos}},
		}
	}

	// Сортировка зависит от того, что за лента.
	//
	// Общая дискавери-лента ранжирует по рейтингу и свежести — там юзер
	// смотрит разных спецов, и порядок внутри одного из них неважен.
	//
	// Лента одного специалиста (открыта с его страницы кликом по работе) —
	// другой сценарий: юзер уже выбрал человека и листает его портфолио.
	// Здесь порядок обязан совпадать со страницей, иначе после закреплённой
	// работы свайп уводит в произвольную последовательность. Поэтому
	// сначала промо, дальше — порядок, который специалист выставил сам.
	sort := []any{
		map[string]any{"rating_avg": map[string]any{"order": "desc"}},
		map[string]any{"video_created_at": map[string]any{"order": "desc"}},
		map[string]any{"video_id": map[string]any{"order": "asc"}},
	}
	if len(q.UserIDs) == 1 {
		sort = []any{
			map[string]any{"is_featured": map[string]any{"order": "desc"}},
			map[string]any{"sort_order": map[string]any{"order": "asc"}},
			map[string]any{"video_created_at": map[string]any{"order": "desc"}},
			// video_id последним — тай-брейк, без него search_after
			// недетерминирован при равных sort_order.
			map[string]any{"video_id": map[string]any{"order": "asc"}},
		}
	}

	// search_after обязан совпадать с sort по количеству значений. У двух
	// веток сортировки арность разная (3 против 4), поэтому курсор, снятый
	// в одной ленте и присланный в другую, уронил бы запрос в ES-ошибку и
	// 502 у юзера. Такой курсор просто игнорируем — «начни сначала», ровно
	// как поступаем со старыми v1-курсорами.
	if len(cursor.SearchAfter) > 0 && len(cursor.SearchAfter) != len(sort) {
		cursor.SearchAfter = nil
	}

	body := map[string]any{
		"size":  pageSize,
		"query": map[string]any{"bool": boolQ},
		"sort":  sort,
		// timeout — server-side лимит OpenSearch. Если shard не успел за
		// 3 сек, возвращаем частичный результат (`timed_out: true` в ответе,
		// но статус всё равно 200). Без этого один медленный shard утаскивал
		// весь запрос в 10-сек HTTP timeout клиента → 5xx на бэке.
		"timeout": "3s",
	}
	if len(cursor.SearchAfter) > 0 {
		body["search_after"] = cursor.SearchAfter
	}
	return body
}

// InterleaveByUser — round-robin по user_id, сохраняя ES-порядок внутри
// каждого пользователя. На вход — ES-выдача (sorted by score), на выход —
// диверсифицированный список: первое видео каждого юзера, потом второе и т.д.
func InterleaveByUser(docs []search.FeedVideoDoc) []search.FeedVideoDoc {
	if len(docs) <= 1 {
		return docs
	}
	byUser := make(map[string][]search.FeedVideoDoc, len(docs))
	order := make([]string, 0, len(docs))
	for _, d := range docs {
		if _, seen := byUser[d.UserID]; !seen {
			order = append(order, d.UserID)
		}
		byUser[d.UserID] = append(byUser[d.UserID], d)
	}
	out := make([]search.FeedVideoDoc, 0, len(docs))
	for round := 0; ; round++ {
		advanced := false
		for _, uid := range order {
			bucket := byUser[uid]
			if round < len(bucket) {
				out = append(out, bucket[round])
				advanced = true
			}
		}
		if !advanced {
			break
		}
	}
	return out
}

func imagesFromDoc(d search.FeedVideoDoc) []Image {
	if len(d.Images) == 0 {
		return nil
	}
	out := make([]Image, 0, len(d.Images))
	for _, im := range d.Images {
		out = append(out, Image{URL: im.ImageURL, Width: im.Width, Height: im.Height})
	}
	return out
}

func videoFromDoc(d search.FeedVideoDoc) Video {
	id, _ := uuid.Parse(d.VideoID) // битый id невозможен (мы сами писали из portfolio_items.id UUID)
	return Video{
		ID:               id,
		URL:              d.VideoURL,
		PreviewURL:       d.PreviewURL,
		AnimatedThumbURL: d.AnimatedThumbURL,
		Thumb:            d.ThumbURL,
		Title:            d.Title,
		Description:      d.Description,
		DurationSec:      d.DurationSec,
		Aspect:           d.Aspect,
		CreatedAt:        d.VideoCreatedAt,
	}
}

func specFromFeedDoc(d search.FeedVideoDoc) Specialist {
	return Specialist{
		UserID:          d.UserID,
		DisplayName:     d.DisplayName,
		AvatarURL:       d.AvatarURL,
		Bio:             d.Bio,
		City:            d.City,
		RateMin:         d.RateMin,
		RateMax:         d.RateMax,
		Currency:        d.Currency,
		Categories:      d.Categories,
		PrimaryCategory: d.PrimaryCategory,
		RatingAvg:       d.RatingAvg,
		ReviewsCount:    d.ReviewsCount,
		ProductionName:  d.ProductionName,
		IsFreelance:     d.IsFreelance,
	}
}
