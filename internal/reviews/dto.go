package reviews

import (
	"time"

	"github.com/google/uuid"
)

type Review struct {
	ID           uuid.UUID  `json:"id"`
	LeadID       *uuid.UUID `json:"lead_id,omitempty"`
	AuthorUserID uuid.UUID  `json:"author_user_id"`
	AuthorName   string     `json:"author_name"`
	TargetUserID uuid.UUID  `json:"target_user_id"`
	Rating       int        `json:"rating"`
	Text         string     `json:"text"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// CreateInput — поля, которые сервис принимает от хендлера.
// AuthorName здесь нет: имя резолвится в Repo.Create по AuthorUserID,
// чтобы исключить подмену из тела запроса (data-sec D7).
type CreateInput struct {
	LeadID       *uuid.UUID
	AuthorUserID uuid.UUID
	TargetUserID uuid.UUID
	Rating       int
	Text         string
}

type UpdateInput struct {
	Rating    *int       `json:"rating"`
	Text      *string    `json:"text"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}
