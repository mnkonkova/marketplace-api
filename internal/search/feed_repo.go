package search

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// FeedVideoDoc — один документ в индексе feed_videos.
// Денормализованный набор полей специалиста + полей видео.
type FeedVideoDoc struct {
	VideoID        string    `json:"video_id"`
	VideoURL       string    `json:"video_url"`
	ThumbURL       string    `json:"thumb_url,omitempty"`
	Title          string    `json:"title,omitempty"`
	Description    string    `json:"description,omitempty"`
	DurationSec    *int      `json:"duration_sec,omitempty"`
	Aspect         string    `json:"aspect,omitempty"`
	VideoCreatedAt time.Time `json:"video_created_at"`
	CategoryCodes  []string  `json:"category_codes"`

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
}

// LoadFeedVideoDocs — собирает все видео-доки одного спеца. Возвращает
// nil-slice если спец не публикуется или у него нет видео — индексер тогда
// просто удалит всё его из feed_videos.
func (r *Repo) LoadFeedVideoDocs(ctx context.Context, userID uuid.UUID) ([]FeedVideoDoc, error) {
	const q = `
SELECT
  pi.id::text,
  COALESCE(pi.video_url, ''),
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
  p.is_published
FROM specialist_profiles p
JOIN portfolio_items pi ON pi.user_id = p.user_id
WHERE p.user_id = $1
  AND p.is_published = TRUE
  AND pi.kind = 'video'
  AND pi.video_url IS NOT NULL AND pi.video_url <> ''
ORDER BY pi.sort_order, pi.created_at DESC`

	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("query feed videos: %w", err)
	}
	defer rows.Close()

	out := make([]FeedVideoDoc, 0, 8)
	for rows.Next() {
		var d FeedVideoDoc
		var dur *int
		if err := rows.Scan(
			&d.VideoID, &d.VideoURL, &d.ThumbURL,
			&d.Title, &d.Description,
			&dur, &d.Aspect,
			&d.VideoCreatedAt, &d.CategoryCodes,
			&d.UserID, &d.DisplayName, &d.AvatarURL,
			&d.Bio, &d.City,
			&d.RateMin, &d.RateMax, &d.Currency,
			&d.Categories, &d.PrimaryCategory,
			&d.RatingAvg, &d.ReviewsCount,
			&d.IsPublished,
		); err != nil {
			return nil, fmt.Errorf("scan feed video: %w", err)
		}
		d.DurationSec = dur
		out = append(out, d)
	}
	return out, rows.Err()
}

// LoadPublishedSpecialistIDs — для bootstrap'а индекса feed_videos: список
// всех опубликованных спецов. Worker дёргает ReconcileVideos для каждого
// при первом запуске, если индекс пуст.
func (r *Repo) LoadPublishedSpecialistIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM specialist_profiles WHERE is_published = TRUE`)
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
