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
	UpdatedAt     time.Time `json:"updated_at"`
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
	// UpdatedAt — версия профиля для optimistic locking.
	// Клиент должен прислать это значение обратно в PatchInput.UpdatedAt,
	// чтобы защититься от lost-update при параллельных PATCH'ах.
	UpdatedAt     time.Time `json:"updated_at"`
	// Контакты для прямой связи. Возвращаются только владельцу профиля
	// (через /me/profile) и менеджеру после создания заявки (см. /leads).
	// В публичные DTO (PublicProfile, search.IndexDoc, feed.Specialist) НЕ
	// попадают — это видимость не для feed'а.
	ContactEmail string `json:"contact_email,omitempty"`
	ContactPhone string `json:"contact_phone,omitempty"`
}

type PatchInput struct {
	DisplayName  *string `json:"display_name"`
	Bio          *string `json:"bio"`
	AvatarURL    *string `json:"avatar_url"`
	City         *string `json:"city"`
	RateMin      *int    `json:"rate_min"`
	RateMax      *int    `json:"rate_max"`
	Currency     *string `json:"currency"`
	ContactEmail *string `json:"contact_email"`
	ContactPhone *string `json:"contact_phone"`
	// UpdatedAt — если задан, в UPDATE добавляется AND updated_at = $X.
	// Несовпадение → 409 conflict (кто-то параллельно отредактировал).
	// Без поля — старый небезопасный поведение для обратной совместимости.
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

type SetCategoriesInput struct {
	Codes   []string `json:"codes"`
	Primary string   `json:"primary"`
	// UpdatedAt — optimistic-lock parent specialist_profiles.updated_at.
	// Если задан — несовпадение → 409. Без поля — старое поведение.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type SetSkillsInput struct {
	SkillIDs []string `json:"skill_ids"`
	// UpdatedAt — см. SetCategoriesInput.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// PortfolioCreateInput — добавление видео в портфолио. URL-форма (юзер сам
// хостит mp4) и file-upload (presigned PUT в YC) идут одной ручкой —
// клиент после аплоада в YC шлёт сюда полученный public_url.
type PortfolioCreateInput struct {
	VideoURL      string   `json:"video_url"`
	ThumbnailURL  string   `json:"thumbnail_url,omitempty"`
	Title         string   `json:"title"`
	Description   string   `json:"description,omitempty"`
	CategoryCodes []string `json:"category_codes,omitempty"`
	DurationSec   int      `json:"duration_sec,omitempty"`
	Aspect        string   `json:"aspect,omitempty"`
}

// PortfolioSetCategoriesInput — обновление списка категорий у видео.
// Пустой массив = «убрать все». Категории должны быть ⊆ категорий профиля.
type PortfolioSetCategoriesInput struct {
	Codes []string `json:"codes"`
	// UpdatedAt — optimistic-lock версия portfolio_items.updated_at.
	// Несовпадение → 409. Без поля — старое поведение.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// PortfolioUploadURLInput — запрос на presigned PUT для прямого аплоада в S3.
type PortfolioUploadURLInput struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// ImageUploadURLInput — запрос на presigned PUT для аплоада картинки
// (аватар или превью к видео). Один эндпоинт обслуживает оба кейса —
// семантика «куда положили» живёт на клиенте: фронт сохраняет полученный
// public_url в profile.avatar_url или portfolio_items.thumbnail_url.
type ImageUploadURLInput struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// PortfolioUploadURL — ответ с URL для PUT и финальным URL для сохранения
// в portfolio_items.video_url (после успешного аплоада клиент зовёт
// POST /me/portfolio с этим public_url).
type PortfolioUploadURL struct {
	UploadURL string `json:"upload_url"`
	PublicURL string `json:"public_url"`
	Key       string `json:"key"`
	ExpiresIn int    `json:"expires_in"` // секунды
}
