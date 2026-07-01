package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("specialist not found")

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

type IndexDoc struct {
	UserID          string     `json:"user_id"`
	DisplayName     string     `json:"display_name"`
	Bio             string     `json:"bio"`
	AvatarURL       string     `json:"avatar_url,omitempty"`
	City            string     `json:"city,omitempty"`
	Categories      []string   `json:"categories"`
	PrimaryCategory string     `json:"primary_category,omitempty"`
	SkillSlugs      []string   `json:"skill_slugs"`
	SkillTitles     string     `json:"skill_titles"`
	// CategoryTitles — русские названия категорий спеца одной строкой
	// («Монтажёр Моушн-дизайнер»). Индексируется под ru_en с synonyms,
	// чтобы multi_match находил спецов по человеческому запросу вроде
	// «моушн-дизайнер» / «сценарист» — категории в keyword'е ищутся
	// только точным match'ем по slug'у.
	CategoryTitles  string     `json:"category_titles"`
	RateMin         *int       `json:"rate_min,omitempty"`
	RateMax         *int       `json:"rate_max,omitempty"`
	Currency        string     `json:"currency"`
	RatingAvg       float64    `json:"rating_avg"`
	ReviewsCount    int        `json:"reviews_count"`
	IsPublished     bool       `json:"is_published"`
	// ModerationStatus — pending_review|approved|rejected. В каталог попадают
	// только approved (см. Indexer.Reconcile). Это поле НЕ уезжает в ES
	// (json:"-") — проверка делается на Go-уровне сразу после загрузки из
	// БД, mapping.go не трогаем.
	ModerationStatus string    `json:"-"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// LastVideoAt — MAX(created_at) видео-айтемов спеца. nil если видео нет.
	// Используется /feed для tie-breaker'а после rating_avg.
	LastVideoAt     *time.Time `json:"last_video_at,omitempty"`
	// PreviewVideoURL / PreviewThumbURL / PreviewAnimatedURL — денормализованное
	// последнее опубликованное видео спеца для рендера карточки в
	// SearchResultsPage. Правила отбора:
	//   • preview_video_url — берём preview_url (480p ~500KB для autoplay);
	//     если пусто (preview ещё не готов) — оригинал video_url.
	//   • preview_thumb_url — thumbnail_url (poster).
	//   • preview_animated_url — animated_thumb_url (animated WebP для
	//     iOS Low Power Mode / soft-limit на конкурентные <video>).
	// Все пустые если у спеца нет видео.
	PreviewVideoURL    string `json:"preview_video_url,omitempty"`
	PreviewThumbURL    string `json:"preview_thumb_url,omitempty"`
	PreviewAnimatedURL string `json:"preview_animated_url,omitempty"`
	// ProductionName / IsFreelance — где работает спец, для отображения в
	// поиске и feed. Производственное имя резолвится через LEFT JOIN
	// productions (пусто если деактивирован/не выбран).
	ProductionName string `json:"production_name,omitempty"`
	IsFreelance    bool   `json:"is_freelance"`
}

func (r *Repo) LoadDoc(ctx context.Context, userID uuid.UUID) (IndexDoc, error) {
	// pv-subquery берёт «последнее опубликованное видео» одним lateral join'ом.
	// Определение «последнее» — MAX(sort_order) по возрастанию + created_at DESC
	// (порядок как в listPortfolio для UI-консистенции). Если у спеца
	// preview_url готов — используем его; иначе fallback на оригинальный
	// video_url. thumbnail_url и animated_thumb_url — best-effort, могут быть ''.
	const q = `
SELECT
  p.user_id::text,
  p.display_name,
  p.bio,
  COALESCE(p.avatar_url, ''),
  COALESCE(p.city, ''),
  COALESCE((SELECT array_agg(category_code) FROM specialist_categories WHERE user_id = p.user_id), ARRAY[]::text[]),
  COALESCE((SELECT category_code FROM specialist_categories WHERE user_id = p.user_id AND is_primary LIMIT 1), ''),
  COALESCE((SELECT array_agg(s.slug) FROM specialist_skills ss JOIN skills s ON s.id = ss.skill_id WHERE ss.user_id = p.user_id), ARRAY[]::text[]),
  COALESCE((SELECT string_agg(s.title, ' ') FROM specialist_skills ss JOIN skills s ON s.id = ss.skill_id WHERE ss.user_id = p.user_id), ''),
  COALESCE((SELECT string_agg(sc.title, ' ')
              FROM specialist_categories ss2
              JOIN specialty_categories sc ON sc.code = ss2.category_code
             WHERE ss2.user_id = p.user_id), ''),
  p.rate_min, p.rate_max, p.currency,
  p.rating_avg, p.reviews_count,
  p.is_published, p.moderation_status, p.updated_at,
  (SELECT MAX(created_at) FROM portfolio_items
     WHERE user_id = p.user_id AND kind = 'video'
       AND video_url IS NOT NULL AND video_url <> ''),
  COALESCE(pv.preview_url_or_video, ''),
  COALESCE(pv.thumbnail_url, ''),
  COALESCE(pv.animated_thumb_url, ''),
  COALESCE(pr.name, ''), p.is_freelance
FROM specialist_profiles p
LEFT JOIN productions pr ON pr.id = p.production_id AND pr.is_active = TRUE
LEFT JOIN LATERAL (
  SELECT
    NULLIF(preview_url, '') IS NOT NULL AS has_preview,
    CASE WHEN NULLIF(preview_url, '') IS NOT NULL
      THEN preview_url ELSE video_url END AS preview_url_or_video,
    thumbnail_url,
    animated_thumb_url
  FROM portfolio_items
  WHERE user_id = p.user_id
    AND kind = 'video'
    AND video_url IS NOT NULL AND video_url <> ''
  ORDER BY sort_order, created_at DESC
  LIMIT 1
) pv ON TRUE
WHERE p.user_id = $1`
	var d IndexDoc
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&d.UserID, &d.DisplayName, &d.Bio, &d.AvatarURL, &d.City,
		&d.Categories, &d.PrimaryCategory,
		&d.SkillSlugs, &d.SkillTitles, &d.CategoryTitles,
		&d.RateMin, &d.RateMax, &d.Currency,
		&d.RatingAvg, &d.ReviewsCount,
		&d.IsPublished, &d.ModerationStatus, &d.UpdatedAt,
		&d.LastVideoAt,
		&d.PreviewVideoURL, &d.PreviewThumbURL, &d.PreviewAnimatedURL,
		&d.ProductionName, &d.IsFreelance,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return IndexDoc{}, ErrNotFound
	}
	if err != nil {
		return IndexDoc{}, fmt.Errorf("load index doc: %w", err)
	}
	return d, nil
}
