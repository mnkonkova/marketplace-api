package projects

import (
	"time"

	"github.com/google/uuid"
)

// ProjectStatus — состояние проекта в целом. Маппинг 1-к-1 с enum
// project_status в БД.
type ProjectStatus string

const (
	StatusDraft     ProjectStatus = "draft"
	StatusActive    ProjectStatus = "active"
	StatusOnHold    ProjectStatus = "on_hold"
	StatusDone      ProjectStatus = "done"
	StatusCancelled ProjectStatus = "cancelled"
	StatusDispute   ProjectStatus = "dispute"
)

// StepStatus — состояние шага. waiting_client — отдельный статус, не
// подвид in_progress; skipped — для опциональных шагов с тайм-аутом.
type StepStatus string

const (
	StepPending       StepStatus = "pending"
	StepInProgress    StepStatus = "in_progress"
	StepWaitingClient StepStatus = "waiting_client"
	StepDone          StepStatus = "done"
	StepRejected      StepStatus = "rejected"
	StepSkipped       StepStatus = "skipped"
)

// ProjectSource — откуда пришёл проект. marketplace = из лида с
// витрины (lead_id != NULL), остальные три — «свои» клиенты.
type ProjectSource string

const (
	SourceMarketplace     ProjectSource = "marketplace"
	SourceManual          ProjectSource = "manual"
	SourceReferral        ProjectSource = "referral"
	SourceReturningClient ProjectSource = "returning_client"
)

// StepOwner — кто отвечает за шаг в воронке. Используется как фильтр
// «requires-client-action», когда показываем CTA в кабинете клиента.
type StepOwner string

const (
	OwnerClient StepOwner = "client"
	OwnerTeam   StepOwner = "team"
	OwnerSystem StepOwner = "system"
)

// ProjectAdminView — полная форма для admin/Directus. Все поля включая
// budget, notes и assigned_to_user_id.
type ProjectAdminView struct {
	ID                uuid.UUID     `json:"id"`
	LeadID            *uuid.UUID    `json:"lead_id,omitempty"`
	ClientUserID      uuid.UUID     `json:"client_user_id"`
	SpecialistUserID  *uuid.UUID    `json:"specialist_user_id,omitempty"`
	AssignedToUserID  *uuid.UUID    `json:"assigned_to_user_id,omitempty"`
	TemplateID        uuid.UUID     `json:"template_id"`
	Title             string        `json:"title"`
	Source            ProjectSource `json:"source"`
	Status            ProjectStatus `json:"status"`
	RevisionsIncluded int           `json:"revisions_included"`
	RevisionsUsed     int           `json:"revisions_used"`
	Budget            *int          `json:"budget,omitempty"`
	Notes             string        `json:"notes,omitempty"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
	CompletedAt       *time.Time    `json:"completed_at,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// ProjectClientView — то, что видит сам клиент в /me/projects/{id}.
// Минус budget (бюджет — внутренняя кухня), минус notes (внутренние
// заметки менеджера), минус assigned_to (клиент не видит «своего»
// менеджера по имени, чтобы не персонифицировать управление).
type ProjectClientView struct {
	ID                uuid.UUID     `json:"id"`
	LeadID            *uuid.UUID    `json:"lead_id,omitempty"`
	SpecialistUserID  *uuid.UUID    `json:"specialist_user_id,omitempty"`
	Title             string        `json:"title"`
	Status            ProjectStatus `json:"status"`
	RevisionsIncluded int           `json:"revisions_included"`
	RevisionsUsed     int           `json:"revisions_used"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
	CompletedAt       *time.Time    `json:"completed_at,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// ProjectSpecialistView — то, что видит назначенный специалист.
// Read-only по варианту А: специалист только смотрит. budget скрыт
// (договорённость между продакшеном и клиентом — не специалиста);
// revisions_used виден, чтобы понимать «сколько раз клиент уже правил».
type ProjectSpecialistView struct {
	ID                uuid.UUID     `json:"id"`
	ClientUserID      uuid.UUID     `json:"client_user_id"`
	Title             string        `json:"title"`
	Status            ProjectStatus `json:"status"`
	RevisionsIncluded int           `json:"revisions_included"`
	RevisionsUsed     int           `json:"revisions_used"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
	CompletedAt       *time.Time    `json:"completed_at,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// StageView — общий формат стадии. Используется и в client-, и в
// specialist-, и в admin-выдаче. Различие только в наборе шагов.
type StageView struct {
	ID          uuid.UUID  `json:"id"`
	Code        string     `json:"code"`
	Title       string     `json:"title"`
	SortOrder   int        `json:"sort_order"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Steps       []StepView `json:"steps"`
}

// StepView — общий формат шага. Поля visible_to_client / visible_to_specialist
// в выдаче НЕ возвращаем — фильтр применяется на стороне сервера. weight
// нужен фронту для расчёта прогресса.
type StepView struct {
	ID           uuid.UUID  `json:"id"`
	Code         string     `json:"code"`
	Title        string     `json:"title"`
	Owner        StepOwner  `json:"owner"`
	Status       StepStatus `json:"status"`
	DurationDays int        `json:"duration_days"`
	Weight       int        `json:"weight"`
	SortOrder    int        `json:"sort_order"`
	ETADate      *time.Time `json:"eta_date,omitempty"`
	CTAPayload   []byte     `json:"cta_payload,omitempty"` // raw JSON
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// CreateManualInput — POST /admin/projects (source = manual|referral|
// returning_client). lead_id всегда NULL — для маркетплейс-проектов
// есть отдельный CreateFromRecipientInput.
type CreateManualInput struct {
	ClientUserID     uuid.UUID     `json:"client_user_id"`
	SpecialistUserID *uuid.UUID    `json:"specialist_user_id,omitempty"`
	AssignedToUserID *uuid.UUID    `json:"assigned_to_user_id,omitempty"`
	TemplateCode     string        `json:"template_code"`    // "video_production"
	TemplateVersion  int           `json:"template_version"` // 1
	Title            string        `json:"title"`
	Source           ProjectSource `json:"source"` // manual|referral|returning_client
	Budget           *int          `json:"budget,omitempty"`
	Notes            string        `json:"notes,omitempty"`
}

// CreateFromRecipientInput — POST /admin/projects/from_recipient/...
// Source принудительно marketplace; recipient мапится 1-к-1.
type CreateFromRecipientInput struct {
	LeadID           uuid.UUID  `json:"lead_id"`
	SpecialistUserID uuid.UUID  `json:"specialist_user_id"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id,omitempty"`
	TemplateCode     string     `json:"template_code"`
	TemplateVersion  int        `json:"template_version"`
	Title            string     `json:"title"`
	Budget           *int       `json:"budget,omitempty"`
	Notes            string     `json:"notes,omitempty"`
}

// PatchAdminInput — PATCH /admin/projects/{id} (optimistic lock через
// UpdatedAt). Поддерживает изменение whitelist-полей: title, budget,
// notes, assigned_to_user_id, status.
type PatchAdminInput struct {
	Title            *string        `json:"title,omitempty"`
	Budget           *int           `json:"budget,omitempty"`
	Notes            *string        `json:"notes,omitempty"`
	AssignedToUserID *uuid.UUID     `json:"assigned_to_user_id,omitempty"`
	Status           *ProjectStatus `json:"status,omitempty"`
	UpdatedAt        time.Time      `json:"updated_at"` // обязателен — защита от lost-update
}
