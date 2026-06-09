package feed

import (
	"time"

	"github.com/google/uuid"
)

// Video — то, что играется в плеере. Держим только то, что реально нужно
// фронту в ленте; описание/категории портфолио-айтема в overlay'е не
// показываем (там уже есть bio спеца).
//
// PreviewURL — облегчённая версия (480p, ~500KB, 5-10 сек, loop) для
// autoplay в карточке. Фронт сначала смотрит preview_url; если пусто —
// фолбэк на url (это значит preview ещё не сгенерился или провалился,
// см. docs/VIDEO_TRANSCODING.md). При клике на «развернуть» фронт
// переключается на url с controls.
type Video struct {
	ID           uuid.UUID `json:"id"`
	URL          string    `json:"url"`
	PreviewURL   string    `json:"preview_url,omitempty"`
	// AnimatedThumbURL — animated WebP для главной (hero + works grid).
	// Autoplay через <img> даже в iOS Low Power Mode (не <video>).
	// Если пусто — фронт фолбэчит на <video preview_url>. См. §11 docs.
	AnimatedThumbURL string `json:"animated_thumb_url,omitempty"`
	Thumb        string    `json:"thumb,omitempty"`
	Title        string    `json:"title,omitempty"`
	Description  string    `json:"description,omitempty"`
	DurationSec  *int      `json:"duration_sec,omitempty"`
	Aspect       string    `json:"aspect,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Specialist — компактная проекция профиля для overlay'я. Это подмножество
// search.IndexDoc + чуть больше (bio).
type Specialist struct {
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
	// ProductionName — название студии или пусто. IsFreelance — флаг
	// фрилансера. Фронт overlay: name > "Фриланс" > (ничего).
	ProductionName  string   `json:"production_name,omitempty"`
	IsFreelance     bool     `json:"is_freelance"`
}

// Item — одна позиция в ленте: видео + контекст специалиста + индекс этого
// видео среди роликов спеца, чтобы показать «1/3» в overlay.
type Item struct {
	Video      Video      `json:"video"`
	Specialist Specialist `json:"specialist"`
	VideoIdx   int        `json:"video_idx"`
	VideoTotal int        `json:"video_total"`
}

// Result — страница ленты. NextCursor пустой → больше нет.
type Result struct {
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Total      int    `json:"total,omitempty"` // сколько спецов под фильтрами всего (ES total)
}

// Query — то, что приходит из URL. Аналогично search.Query, но с курсором
// вместо offset (round-robin несимметричен относительно offset).
//
// UserIDs — жёсткий список спецов: если непустой, фильтры Q/Categories/
// SkillSlugs/City игнорируются (юзер уже выбрал кого смотреть на предыдущем
// шаге — например, через /search или /search/summarize). Лимит ~100.
type Query struct {
	Q             string
	Categories    []string
	SkillSlugs    []string
	City          string
	Cursor        string
	PerSpecialist int          // максимум видео на одного спеца, default 5
	UserIDs       []uuid.UUID  // если непустой — жёсткий фильтр по этим спецам
}
