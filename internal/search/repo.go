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
	UserID          string    `json:"user_id"`
	DisplayName     string    `json:"display_name"`
	Bio             string    `json:"bio"`
	AvatarURL       string    `json:"avatar_url,omitempty"`
	City            string    `json:"city,omitempty"`
	Categories      []string  `json:"categories"`
	PrimaryCategory string    `json:"primary_category,omitempty"`
	SkillSlugs      []string  `json:"skill_slugs"`
	SkillTitles     string    `json:"skill_titles"`
	RateMin         *int      `json:"rate_min,omitempty"`
	RateMax         *int      `json:"rate_max,omitempty"`
	Currency        string    `json:"currency"`
	RatingAvg       float64   `json:"rating_avg"`
	ReviewsCount    int       `json:"reviews_count"`
	IsPublished     bool      `json:"is_published"`
	UpdatedAt       time.Time `json:"updated_at"`
	// LastVideoAt — MAX(created_at) видео-айтемов спеца. nil если видео нет.
	// Используется /feed для tie-breaker'а после rating_avg.
	LastVideoAt *time.Time `json:"last_video_at,omitempty"`
}

func (r *Repo) LoadDoc(ctx context.Context, userID uuid.UUID) (IndexDoc, error) {
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
  p.rate_min, p.rate_max, p.currency,
  p.rating_avg, p.reviews_count,
  p.is_published, p.updated_at,
  (SELECT MAX(created_at) FROM portfolio_items
     WHERE user_id = p.user_id AND kind = 'video'
       AND video_url IS NOT NULL AND video_url <> '')
FROM specialist_profiles p
WHERE p.user_id = $1`
	var d IndexDoc
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&d.UserID, &d.DisplayName, &d.Bio, &d.AvatarURL, &d.City,
		&d.Categories, &d.PrimaryCategory,
		&d.SkillSlugs, &d.SkillTitles,
		&d.RateMin, &d.RateMax, &d.Currency,
		&d.RatingAvg, &d.ReviewsCount,
		&d.IsPublished, &d.UpdatedAt,
		&d.LastVideoAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return IndexDoc{}, ErrNotFound
	}
	if err != nil {
		return IndexDoc{}, fmt.Errorf("load index doc: %w", err)
	}
	return d, nil
}
