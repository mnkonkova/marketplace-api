package projects

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Ветки переписки в проекте. Их три, и разделены они не оформлением, а
// правом читать:
//
//	ThreadClient   — клиент ↔ менеджер. В общем проекте (kind='general')
//	                 менеджера нет, и в этой же ветке с клиентом говорит
//	                 исполнитель: другого места для разговора там просто нет.
//	ThreadCreator  — конкретный креатор ↔ менеджер. Ветка адресная: ни клиент,
//	                 ни соседний креатор её не видят.
//	ThreadInternal — только менеджеры и админы.
const (
	ThreadClient   = "client"
	ThreadCreator  = "creator"
	ThreadInternal = "internal"
)

// Форматы тела. plain — текст как есть; html — размеченное сообщение,
// прошедшее через internal/richtext. Третье значение схемы (tiptap_json)
// осталось от задела и на входе не принимается.
const (
	FormatPlain = "plain"
	FormatHTML  = "html"
)

// Comment — запись из project_comments.
type Comment struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	AuthorID  uuid.UUID `json:"author_id"`
	// AuthorName — display_name автора: specialist_profile → client_profile →
	// префикс email. Считается в запросе, в таблице комментариев его нет.
	AuthorName string `json:"author_name,omitempty"`
	// Body — тело в формате BodyFormat. Для html здесь уже очищенная
	// разметка: чистка происходит на записи, читателю отдаётся готовое.
	Body       string `json:"body"`
	BodyFormat string `json:"body_format"`
	// BodyText — то же без разметки. Фронту нужен там, где HTML не к месту:
	// заголовок уведомления, превью в списке, поиск по переписке.
	BodyText string `json:"body_text"`
	// Thread/ThreadUserID — ветка. ThreadUserID заполнен только у creator.
	Thread       string     `json:"thread"`
	ThreadUserID *uuid.UUID `json:"thread_user_id,omitempty"`
	// IsInternal — то же, что Thread == ThreadInternal. Остаётся в ответе:
	// на него смотрит уже написанный фронт.
	IsInternal bool `json:"is_internal"`
	// Mentions — кого упомянули. Пусто, если никого.
	Mentions  []uuid.UUID `json:"mentions,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	DeletedAt *time.Time  `json:"deleted_at,omitempty"`
}

// CreateCommentInput — что кладём в project_comments. Тело сюда приезжает
// уже очищенным (см. Service.CreateComment): репозиторий разметку не
// разбирает и не проверяет.
type CreateCommentInput struct {
	ProjectID    uuid.UUID
	AuthorID     uuid.UUID
	Thread       string
	ThreadUserID *uuid.UUID
	Body         string
	BodyText     string
	BodyFormat   string
	Mentions     []uuid.UUID
}

// Participant — участник ветки: тот, кто её читает. Он же — кандидат в
// упоминания, и это одно и то же множество не случайно: упомянуть можно
// только того, кто сообщение и так увидит.
type Participant struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"display_name"`
	// Role — client | creator | specialist | manager | admin. Нужна фронту,
	// чтобы показать, кто есть кто в выпадашке @.
	Role string `json:"role"`
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
