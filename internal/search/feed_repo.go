package search

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// FeedImageDoc — один кадр photo-set'а внутри FeedVideoDoc (Kind='image').
// Кадры идут уже отсортированными по sort_order — фронт рендерит как есть.
type FeedImageDoc struct {
	ImageURL string `json:"image_url"`
	Width    *int   `json:"width,omitempty"`
	Height   *int   `json:"height,omitempty"`
}

// FeedVideoDoc — один документ в индексе feed_videos. Семантически «item»:
// либо одно видео (Kind='video'), либо photo-set из N кадров (Kind='image').
// Имя «FeedVideoDoc» сохранили для back-compat с индексом feed_videos.
// Денормализованный набор полей специалиста + полей айтема.
type FeedVideoDoc struct {
	// Kind — 'video' | 'image'. Для image: VideoURL/PreviewURL/AnimatedThumb
	// будут "", Images содержит карусель.
	Kind           string    `json:"kind"`
	VideoID        string    `json:"video_id"`
	VideoURL       string    `json:"video_url"`
	// PreviewURL — маленький 480p ~500KB вариант для autoplay в фиде.
	// Пусто, если preview_status != 'ready' (фронт фолбэчит на VideoURL).
	PreviewURL     string    `json:"preview_url,omitempty"`
	// AnimatedThumbURL — animated WebP «гифка» (~50-150KB) для autoplay
	// на главной через <img>. Решает iOS LPM-блокировку. См. §11 docs.
	AnimatedThumbURL string  `json:"animated_thumb_url,omitempty"`
	ThumbURL       string    `json:"thumb_url,omitempty"`
	Title          string    `json:"title,omitempty"`
	Description    string    `json:"description,omitempty"`
	DurationSec    *int      `json:"duration_sec,omitempty"`
	Aspect         string    `json:"aspect,omitempty"`
	VideoCreatedAt time.Time `json:"video_created_at"`
	CategoryCodes  []string  `json:"category_codes"`
	// Images — для Kind='image' карусель кадров, упорядочена по sort_order.
	Images         []FeedImageDoc `json:"images,omitempty"`

	UserID          string   `json:"user_id"`
	DisplayName     string   `json:"display_name"`
	AvatarURL       string   `json:"avatar_url,omitempty"`
	Bio             string   `json:"bio,omitempty"`
	City            string   `json:"city,omitempty"`
	RateMin         *int     `json:"rate_min,omitempty"`
	RateMax         *int     `json:"rate_max,omitempty"`
	Currency        string   `json:"currency,omitempty"`
	Categories      []string `json:"categories"`
	PrimaryCategory string   `json:"primary_category,omitempty"`
	RatingAvg       float64  `json:"rating_avg"`
	ReviewsCount    int      `json:"reviews_count"`
	IsPublished     bool     `json:"is_published"`
	// ProductionName — название студии или "" (фрилансер / не выбрал /
	// студия деактивирована). IsFreelance — флаг фрилансера.
	ProductionName  string   `json:"production_name,omitempty"`
	IsFreelance     bool     `json:"is_freelance"`
}

// LoadFeedVideoDocs — собирает все feed-доки одного спеца: видео + photo-сеты.
// Возвращает nil-slice если спец не публикуется или у него ничего нет —
// индексер тогда просто удалит всё его из feed_videos.
//
// Один SELECT покрывает оба kind'а; для kind='image' докидываем кадры
// отдельным батч-запросом по item_id ∈ собранным.
func (r *Repo) LoadFeedVideoDocs(ctx context.Context, userID uuid.UUID) ([]FeedVideoDoc, error) {
	const q = `
SELECT
  pi.id::text,
  pi.kind,
  COALESCE(pi.video_url, ''),
  COALESCE(pi.preview_url, ''),
  COALESCE(pi.animated_thumb_url, ''),
  COALESCE(pi.thumbnail_url, ''),
  pi.title,
  pi.description,
  pi.duration_sec,
  COALESCE(pi.aspect, ''),
  pi.created_at,
  pi.category_codes,
  p.user_id::text,
  p.display_name,
  COALESCE(p.avatar_url, ''),
  p.bio,
  COALESCE(p.city, ''),
  p.rate_min, p.rate_max, p.currency,
  COALESCE((SELECT array_agg(category_code) FROM specialist_categories WHERE user_id = p.user_id), ARRAY[]::text[]),
  COALESCE((SELECT category_code FROM specialist_categories WHERE user_id = p.user_id AND is_primary LIMIT 1), ''),
  p.rating_avg, p.reviews_count,
  p.is_published,
  COALESCE(pr.name, ''), p.is_freelance
FROM specialist_profiles p
JOIN portfolio_items pi ON pi.user_id = p.user_id
LEFT JOIN productions pr ON pr.id = p.production_id AND pr.is_active = TRUE
WHERE p.user_id = $1
  AND p.is_published = TRUE
  AND p.moderation_status = 'approved'
  AND (
        (pi.kind = 'video' AND pi.video_url IS NOT NULL AND pi.video_url <> '')
     OR  pi.kind = 'image'
      )
ORDER BY pi.sort_order, pi.created_at DESC`

	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("query feed items: %w", err)
	}
	defer rows.Close()

	out := make([]FeedVideoDoc, 0, 8)
	var photoItemIDs []string
	for rows.Next() {
		var d FeedVideoDoc
		var dur *int
		if err := rows.Scan(
			&d.VideoID, &d.Kind, &d.VideoURL, &d.PreviewURL, &d.AnimatedThumbURL, &d.ThumbURL,
			&d.Title, &d.Description,
			&dur, &d.Aspect,
			&d.VideoCreatedAt, &d.CategoryCodes,
			&d.UserID, &d.DisplayName, &d.AvatarURL,
			&d.Bio, &d.City,
			&d.RateMin, &d.RateMax, &d.Currency,
			&d.Categories, &d.PrimaryCategory,
			&d.RatingAvg, &d.ReviewsCount,
			&d.IsPublished,
			&d.ProductionName, &d.IsFreelance,
		); err != nil {
			return nil, fmt.Errorf("scan feed item: %w", err)
		}
		d.DurationSec = dur
		if d.Kind == "image" {
			photoItemIDs = append(photoItemIDs, d.VideoID)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(photoItemIDs) > 0 {
		byItem, err := r.loadFeedImages(ctx, photoItemIDs)
		if err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].Kind == "image" {
				out[i].Images = byItem[out[i].VideoID]
			}
		}
	}
	return out, nil
}

// loadFeedImages — батч-загрузка кадров photo-сетов одного спеца.
// item_id передаются как text (мы их уже привели к string выше).
func (r *Repo) loadFeedImages(ctx context.Context, itemIDs []string) (map[string][]FeedImageDoc, error) {
	rows, err := r.db.Query(ctx, `
SELECT portfolio_item_id::text, image_url, width, height
FROM portfolio_images
WHERE portfolio_item_id = ANY($1::uuid[])
ORDER BY portfolio_item_id, sort_order, created_at`, itemIDs)
	if err != nil {
		return nil, fmt.Errorf("query feed images: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]FeedImageDoc, len(itemIDs))
	for rows.Next() {
		var parent string
		var img FeedImageDoc
		if err := rows.Scan(&parent, &img.ImageURL, &img.Width, &img.Height); err != nil {
			return nil, err
		}
		out[parent] = append(out[parent], img)
	}
	return out, rows.Err()
}

// LoadPublishedSpecialistIDs — для bootstrap'а индекса feed_videos: список
// всех опубликованных И ОДОБРЕННЫХ спецов. Worker дёргает ReconcileVideos
// для каждого при первом запуске, если индекс пуст.
func (r *Repo) LoadPublishedSpecialistIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM specialist_profiles
		 WHERE is_published = TRUE AND moderation_status = 'approved'`)
	if err != nil {
		return nil, fmt.Errorf("load published ids: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, 32)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
