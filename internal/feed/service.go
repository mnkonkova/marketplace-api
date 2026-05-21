package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/search"
)

// специалистов на одну страницу — это «глубина» одного запроса в OpenSearch.
// Видео получится до specialistsPerPage * perSpecialist (default 10*5 = 50).
const specialistsPerPage = 10

// верхняя граница per_specialist, чтобы клиент не утянул всю базу одним запросом.
const maxPerSpecialist = 5

// poolSize — сколько спецов ES-выборки тянем заранее, чтобы локально пере-
// ранжировать (с видео выше, потом по рейтингу). Для текущего MVP-объёма
// (десятки-сотни спецов) это весь корпус. Когда вырастет — заменим on-demand
// reindex'ом с полем videos_count и нативной ES-сортировкой.
const poolSize = 200

// search.Service.Search режет Limit к 50 — внутри feed страничим и сами
// собираем пул нужного размера.
const searchPageSize = 50

type Service struct {
	search *search.Service
	repo   *Repo
	cache  *Cache
}

func NewService(searchSvc *search.Service, repo *Repo) *Service {
	return &Service{search: searchSvc, repo: repo}
}

// WithCache подключает Redis-кэш страниц. nil-safe: без вызова Service
// работает без кэша. main подключает только если redis-клиент доступен.
func (s *Service) WithCache(c *Cache) *Service {
	s.cache = c
	return s
}

type cursorPayload struct {
	// Offset — индекс в локально пересортированном пуле спецов-с-видео.
	Offset int `json:"o"`
	// Seen — user_id'шки, уже отданные в предыдущих страницах. Защита от
	// дублей при параллельных правках портфолио (новое видео переставляет
	// спеца в пуле, без seen клиент бы увидел его дважды). Хранится последние
	// до cursorSeenCap записей, FIFO. Для очень глубоких сессий лимит может
	// не вместить всех — это даст редкий дубль, но не растёт безгранично.
	Seen []string `json:"s,omitempty"`
}

// cursorSeenCap — потолок размера seen-сета в курсоре. ~200 user_id в base64
// JSON ≈ 7-8 KB, безопасно ходить в URL query.
const cursorSeenCap = 200

func (s *Service) Feed(ctx context.Context, q Query) (Result, error) {
	if q.PerSpecialist <= 0 || q.PerSpecialist > maxPerSpecialist {
		q.PerSpecialist = maxPerSpecialist
	}
	// Жёсткий ids-фильтр перекрывает любые soft-фильтры: юзер уже выбрал
	// кого смотреть на предыдущем шаге, не нужно ещё раз срезать по категории
	// (иначе у спеца пропадут видео других категорий, что неожиданно).
	if len(q.UserIDs) > 0 {
		q.Categories = nil
		q.SkillSlugs = nil
		q.City = ""
		q.Q = ""
	}

	// Кэш-чек до тяжёлых запросов в ES/PG. /feed публичный, контента под
	// одну и ту же URL никто не персонализирует — кэшируем по форме запроса.
	if cached, ok := s.cache.Get(ctx, q); ok {
		return cached, nil
	}

	var cursor cursorPayload
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return Result{}, fmt.Errorf("decode cursor: %w", err)
		}
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return Result{}, fmt.Errorf("parse cursor: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(cursor.Seen))
	for _, id := range cursor.Seen {
		seen[id] = struct{}{}
	}

	allDocs, totalMatched, err := s.fetchCandidates(ctx, q)
	if err != nil {
		return Result{}, err
	}
	if len(allDocs) == 0 {
		return Result{Items: []Item{}, Total: totalMatched}, nil
	}

	// 2) батчевый load видео для всего пула.
	userIDs := make([]uuid.UUID, 0, len(allDocs))
	for _, doc := range allDocs {
		uid, err := uuid.Parse(doc.UserID)
		if err != nil {
			continue
		}
		userIDs = append(userIDs, uid)
	}
	videosByUser, err := s.repo.LoadVideosByUsers(ctx, userIDs, q.PerSpecialist, q.Categories)
	if err != nil {
		return Result{}, err
	}

	// 3) фильтр «только с видео» + сортировка по рейтингу. Сохраняем порядок
	//    ES для тех, у кого rating совпадает (sort.SliceStable).
	type ranked struct {
		doc  search.IndexDoc
		vids []Video
	}
	pool := make([]ranked, 0, len(allDocs))
	for _, doc := range allDocs {
		uid, err := uuid.Parse(doc.UserID)
		if err != nil {
			continue
		}
		vids := videosByUser[uid]
		if len(vids) == 0 {
			continue
		}
		// Дедуп по seen — спеца уже видели на предыдущих страницах,
		// не показываем снова даже если пул сместился из-за новых видео.
		if _, dup := seen[doc.UserID]; dup {
			continue
		}
		pool = append(pool, ranked{doc: doc, vids: vids})
	}
	// Ранжирование: rating_avg DESC, потом last_video_at DESC (свежее видео
	// выше), потом стабильный tiebreaker по user_id чтобы порядок между
	// страницами не плавал при равных значениях.
	sort.SliceStable(pool, func(i, j int) bool {
		a, b := pool[i].doc, pool[j].doc
		if a.RatingAvg != b.RatingAvg {
			return a.RatingAvg > b.RatingAvg
		}
		ai := timeOrZero(a.LastVideoAt)
		bi := timeOrZero(b.LastVideoAt)
		if !ai.Equal(bi) {
			return ai.After(bi)
		}
		return a.UserID < b.UserID
	})

	// 4) пагинация: берём первые specialistsPerPage из отфильтрованного пула.
	// Дедуп по seen уже сделан выше, offset больше не нужен.
	if len(pool) == 0 {
		return Result{Items: []Item{}, Total: totalMatched}, nil
	}
	end := specialistsPerPage
	if end > len(pool) {
		end = len(pool)
	}
	page := pool[:end]

	// 5) round-robin внутри страницы: round 0 — первое видео каждого спеца,
	//    round 1 — второе и т.д.
	items := make([]Item, 0, len(page)*q.PerSpecialist)
	for round := 0; round < q.PerSpecialist; round++ {
		for _, r := range page {
			if round >= len(r.vids) {
				continue
			}
			items = append(items, Item{
				Video:      r.vids[round],
				Specialist: specFromDoc(r.doc),
				VideoIdx:   round,
				VideoTotal: len(r.vids),
			})
		}
	}

	out := Result{Items: items, Total: totalMatched}
	// Курсор есть смысл отдавать, только если в пуле осталось ещё что-то
	// после текущей страницы (а в /feed?ids= ещё и есть смысл вообще делать
	// next-page: ids ограничены и могут целиком влезть в одну страницу).
	if end < len(pool) {
		// Новый seen = старый + user_id'шки текущей страницы, с FIFO-отрезанием
		// до cursorSeenCap. На очень длинных сессиях старые id будут выпадать;
		// это даст редкий дубль, но не блокирует пагинацию.
		newSeen := append([]string(nil), cursor.Seen...)
		for _, r := range page {
			newSeen = append(newSeen, r.doc.UserID)
		}
		if over := len(newSeen) - cursorSeenCap; over > 0 {
			newSeen = newSeen[over:]
		}
		next := cursorPayload{Seen: newSeen}
		raw, _ := json.Marshal(next)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	s.cache.Set(ctx, q, out)
	return out, nil
}

// fetchCandidates — кандидаты для ранжирования. Две ветки:
//
//  1. q.UserIDs непустой → жёсткий список (после /search → /feed?ids=...).
//     Один batch-запрос в ES (terms по user_id, is_published=true). Без
//     поискового q/category/city — юзер уже выбрал кого смотреть.
//     totalMatched = сколько фактически нашлось (могло быть меньше len(ids)
//     если кто-то снят с публикации между шагами).
//
//  2. UserIDs пустой → текущее поведение: пул до poolSize спецов под
//     текстовый/категорийный/городской фильтр, страницами по searchPageSize.
func (s *Service) fetchCandidates(ctx context.Context, q Query) ([]search.IndexDoc, int, error) {
	if len(q.UserIDs) > 0 {
		ids := make([]string, len(q.UserIDs))
		for i, id := range q.UserIDs {
			ids[i] = id.String()
		}
		docs, err := s.search.LoadDocsByIDs(ctx, ids)
		if err != nil {
			return nil, 0, fmt.Errorf("load by ids: %w", err)
		}
		return docs, len(docs), nil
	}

	// Skills из AI-флоу clarify: они извлечены LLM из свободного текста и
	// часть спецов их явно не проставила. Чтобы фид симметрично с
	// /search/summarize находил тех же спецов (иначе при тогле «В ленту» из
	// AI-подбора пропадает запиканный кандидат), подмешиваем slug'и в Q и
	// убираем как hard-фильтр. Жёсткие skill-фильтры из UI-чипов прилетают
	// в /specialists и /search напрямую — они тут не задействованы.
	searchQ := q.Q
	if len(q.SkillSlugs) > 0 {
		extra := strings.Join(q.SkillSlugs, " ")
		if searchQ == "" {
			searchQ = extra
		} else {
			searchQ = searchQ + " " + extra
		}
	}

	// Тянем пул спецов под фильтром страницами по searchPageSize, до poolSize.
	// Локально пере-ранжируем: с видео — выше, потом по рейтингу. Альтернатива —
	// хранить videos_count в индексе и сортировать в ES, но это требует
	// реиндексации на каждое изменение портфолио.
	allDocs := make([]search.IndexDoc, 0, poolSize)
	totalMatched := 0
	for off := 0; off < poolSize; off += searchPageSize {
		page, err := s.search.Search(ctx, search.Query{
			Q:          searchQ,
			Categories: q.Categories,
			City:       q.City,
			Limit:      searchPageSize,
			Offset:     off,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("search: %w", err)
		}
		if len(page.Items) == 0 {
			break
		}
		allDocs = append(allDocs, page.Items...)
		totalMatched = page.Total
		if len(allDocs) >= page.Total || len(page.Items) < searchPageSize {
			break
		}
	}
	return allDocs, totalMatched, nil
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func specFromDoc(d search.IndexDoc) Specialist {
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
	}
}
