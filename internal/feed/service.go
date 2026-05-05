package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"marketpclce/internal/search"
)

// специалистов на одну страницу — это «глубина» одного запроса в OpenSearch.
// Видео получится до specialistsPerPage * perSpecialist (default 10*5 = 50).
const specialistsPerPage = 10

// верхняя граница per_specialist, чтобы клиент не утянул всю базу одним запросом.
const maxPerSpecialist = 5

type Service struct {
	search *search.Service
	repo   *Repo
}

func NewService(searchSvc *search.Service, repo *Repo) *Service {
	return &Service{search: searchSvc, repo: repo}
}

type cursorPayload struct {
	SearchOffset int `json:"o"`
}

func (s *Service) Feed(ctx context.Context, q Query) (Result, error) {
	if q.PerSpecialist <= 0 || q.PerSpecialist > maxPerSpecialist {
		q.PerSpecialist = maxPerSpecialist
	}

	offset := 0
	if q.Cursor != "" {
		var c cursorPayload
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return Result{}, fmt.Errorf("decode cursor: %w", err)
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return Result{}, fmt.Errorf("parse cursor: %w", err)
		}
		offset = c.SearchOffset
	}

	// 1) ранжируем спецов как обычно — те же фильтры, та же релаксация.
	searchRes, err := s.search.Search(ctx, search.Query{
		Q:          q.Q,
		Categories: q.Categories,
		SkillSlugs: q.SkillSlugs,
		City:       q.City,
		Limit:      specialistsPerPage,
		Offset:     offset,
	})
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}

	if len(searchRes.Items) == 0 {
		return Result{Items: []Item{}, Total: searchRes.Total}, nil
	}

	// 2) батчевый load видео.
	userIDs := make([]uuid.UUID, 0, len(searchRes.Items))
	for _, doc := range searchRes.Items {
		uid, err := uuid.Parse(doc.UserID)
		if err != nil {
			continue
		}
		userIDs = append(userIDs, uid)
	}
	videosByUser, err := s.repo.LoadVideosByUsers(ctx, userIDs, q.PerSpecialist)
	if err != nil {
		return Result{}, err
	}

	// 3) round-robin: round 0 — первое видео каждого, round 1 — второе, и т.д.
	// Спецов без видео скипаем (UX «постер-слайдов» делается на фронте, отдельным
	// заходом). Если хочется их включить — здесь можно сгенерить «синтетический»
	// видео-айтем без URL.
	items := make([]Item, 0, len(searchRes.Items)*q.PerSpecialist)
	for round := 0; round < q.PerSpecialist; round++ {
		for _, doc := range searchRes.Items {
			uid, err := uuid.Parse(doc.UserID)
			if err != nil {
				continue
			}
			vids := videosByUser[uid]
			if round >= len(vids) {
				continue
			}
			items = append(items, Item{
				Video:      vids[round],
				Specialist: specFromDoc(doc),
				VideoIdx:   round,
				VideoTotal: len(vids),
			})
		}
	}

	out := Result{Items: items, Total: searchRes.Total}
	// Курсор — есть ли смысл идти дальше. Если ES вернул < specialistsPerPage,
	// дальше пусто.
	if len(searchRes.Items) >= specialistsPerPage {
		next := cursorPayload{SearchOffset: offset + specialistsPerPage}
		raw, _ := json.Marshal(next)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
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
