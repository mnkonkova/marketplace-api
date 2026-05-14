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
}

type CreateInput struct {
	LeadID       *uuid.UUID
	AuthorUserID uuid.UUID
	AuthorName   string
	TargetUserID uuid.UUID
	Rating       int
	Text         string
}

type UpdateInput struct {
	Rating *int    `json:"rating"`
	Text   *string `json:"text"`
}
