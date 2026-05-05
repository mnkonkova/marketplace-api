package profiles

import (
	"time"

	"github.com/google/uuid"
)

type CategoryRef struct {
	Code      string `json:"code"`
	Title     string `json:"title"`
	IsPrimary bool   `json:"is_primary"`
}

type SkillRef struct {
	ID    uuid.UUID `json:"id"`
	Slug  string    `json:"slug"`
	Title string    `json:"title"`
	Kind  string    `json:"kind"`
}

type Review struct {
	ID         uuid.UUID `json:"id"`
	AuthorName string    `json:"author_name"`
	Rating     int       `json:"rating"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
}

type PortfolioItem struct {
	ID            uuid.UUID `json:"id"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	VideoURL      string    `json:"video_url,omitempty"`
	ThumbnailURL  string    `json:"thumbnail_url,omitempty"`
	ExternalURL   string    `json:"external_url,omitempty"`
	CategoryCodes []string  `json:"category_codes"`
	SortOrder     int       `json:"sort_order"`
	CreatedAt     time.Time `json:"created_at"`
}

type PublicProfile struct {
	UserID       uuid.UUID       `json:"user_id"`
	DisplayName  string          `json:"display_name"`
	Bio          string          `json:"bio"`
	AvatarURL    string          `json:"avatar_url,omitempty"`
	City         string          `json:"city,omitempty"`
	RateMin      *int            `json:"rate_min,omitempty"`
	RateMax      *int            `json:"rate_max,omitempty"`
	Currency     string          `json:"currency"`
	RatingAvg    float64         `json:"rating_avg"`
	ReviewsCount int             `json:"reviews_count"`
	Categories   []CategoryRef   `json:"categories"`
	Skills       []SkillRef      `json:"skills"`
	Portfolio    []PortfolioItem `json:"portfolio"`
	Reviews      []Review        `json:"reviews"`
}

type Profile struct {
	UserID        uuid.UUID `json:"user_id"`
	DisplayName   string    `json:"display_name"`
	Bio           string    `json:"bio"`
	AvatarURL     string    `json:"avatar_url,omitempty"`
	City          string    `json:"city,omitempty"`
	RateMin       *int      `json:"rate_min,omitempty"`
	RateMax       *int      `json:"rate_max,omitempty"`
	Currency      string    `json:"currency"`
	IsPublished   bool      `json:"is_published"`
	RatingAvg     float64   `json:"rating_avg"`
	ReviewsCount  int       `json:"reviews_count"`
	Categories    []string  `json:"categories"`
	PrimaryCategory string  `json:"primary_category,omitempty"`
	SkillIDs      []string  `json:"skill_ids"`
}

type PatchInput struct {
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
	AvatarURL   *string `json:"avatar_url"`
	City        *string `json:"city"`
	RateMin     *int    `json:"rate_min"`
	RateMax     *int    `json:"rate_max"`
	Currency    *string `json:"currency"`
}

type SetCategoriesInput struct {
	Codes   []string `json:"codes"`
	Primary string   `json:"primary"`
}

type SetSkillsInput struct {
	SkillIDs []string `json:"skill_ids"`
}

// PortfolioCreateInput — добавление видео в портфолио. Сейчас принимаем только
// URL-форму (юзер сам хостит mp4); прямой file-upload через S3 — этап #4b,
// когда будут ключи Yandex Object Storage.
type PortfolioCreateInput struct {
	VideoURL      string   `json:"video_url"`
	ThumbnailURL  string   `json:"thumbnail_url,omitempty"`
	Title         string   `json:"title"`
	Description   string   `json:"description,omitempty"`
	CategoryCodes []string `json:"category_codes,omitempty"`
	DurationSec   int      `json:"duration_sec,omitempty"`
	Aspect        string   `json:"aspect,omitempty"`
}
