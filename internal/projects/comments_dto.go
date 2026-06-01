package projects

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Comment — запись из project_comments. body_format задел под TipTap;
// MVP пишет 'plain'.
type Comment struct {
	ID         uuid.UUID  `json:"id"`
	ProjectID  uuid.UUID  `json:"project_id"`
	AuthorID   uuid.UUID  `json:"author_id"`
	AuthorName string     `json:"author_name,omitempty"`
	Body       string     `json:"body"`
	BodyFormat string     `json:"body_format"`
	IsInternal bool       `json:"is_internal"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
}

// Event — запись из project_step_events для widgets/activity-feed.
// payload — сырой JSON: фронт-виджет сам разбирает по event_kind.
type Event struct {
	ID         int64           `json:"id"`
	ProjectID  uuid.UUID       `json:"project_id"`
	StepID     *uuid.UUID      `json:"step_id,omitempty"`
	ActorID    *uuid.UUID      `json:"actor_user_id,omitempty"`
	ActorName  string          `json:"actor_display_name,omitempty"`
	ActorType  string          `json:"actor_type"`
	EventKind  string          `json:"event_kind"`
	FromStatus *string         `json:"from_status,omitempty"`
	ToStatus   *string         `json:"to_status,omitempty"`
	Comment    string          `json:"comment,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}
