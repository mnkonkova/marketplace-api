package leads

import (
	"time"

	"github.com/google/uuid"
)

type Lead struct {
	ID                 uuid.UUID  `json:"id"`
	ClientUserID       *uuid.UUID `json:"client_user_id,omitempty"`
	ClientName         string     `json:"client_name"`
	ClientContact      string     `json:"client_contact"`
	Brief              string     `json:"brief"`
	BudgetMin          *int       `json:"budget_min,omitempty"`
	BudgetMax          *int       `json:"budget_max,omitempty"`
	Deadline           *time.Time `json:"deadline,omitempty"`
	TargetCategoryCode string     `json:"target_category_code,omitempty"`
	Status             string     `json:"status"`
	CreatedAt          time.Time  `json:"created_at"`
	RecipientCount     int        `json:"recipient_count"`
}

type IncomingLead struct {
	Lead
	RecipientStatus      string     `json:"recipient_status"`
	RecipientRespondedAt *time.Time `json:"recipient_responded_at,omitempty"`
}

type CreateInput struct {
	ClientUserID       *uuid.UUID
	ClientName         string
	ClientContact      string
	Brief              string
	BudgetMin          *int
	BudgetMax          *int
	Deadline           *time.Time
	TargetCategoryCode string
	SpecialistIDs      []uuid.UUID
}

// SpecialistContact — контакты выбранных специалистов, возвращаемые в
// ответе POST /leads. По продуктовому требованию контакты НЕ показываются
// в feed/search/публичном профиле — только менеджеру (тому, кто создал
// заявку) уже после её отправки.
type SpecialistContact struct {
	UserID       uuid.UUID `json:"user_id"`
	DisplayName  string    `json:"display_name"`
	ContactEmail string    `json:"contact_email,omitempty"`
	ContactPhone string    `json:"contact_phone,omitempty"`
}

// CreateResult — id созданной заявки + контакты выбранных спецов.
type CreateResult struct {
	ID          uuid.UUID           `json:"id"`
	Specialists []SpecialistContact `json:"specialists"`
}

const (
	LeadStatusOpen       = "open"
	LeadStatusInProgress = "in_progress"
	LeadStatusClosed     = "closed"
	LeadStatusCancelled  = "cancelled"

	RecipientStatusSent     = "sent"
	RecipientStatusViewed   = "viewed"
	RecipientStatusAccepted = "accepted"
	RecipientStatusDeclined = "declined"
)
