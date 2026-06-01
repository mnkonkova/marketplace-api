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

// ProjectDisplayStatus — вычисленный (computed) статус проекта для UI.
// Зеркало raw `project.status`, но проще: терминальные состояния
// (done/cancelled/on_hold/dispute) маппятся напрямую, активные проекты
// получают статус из агрегированного состояния шагов.
//
// Фронт ТОЛЬКО рендерит этот enum по labels/colors — никакой собственной
// derive-логики не должен.
type ProjectDisplayStatus string

const (
	DisplayStatusNotStarted    ProjectDisplayStatus = "not_started"
	DisplayStatusInProgress    ProjectDisplayStatus = "in_progress"
	DisplayStatusWaitingAction ProjectDisplayStatus = "waiting_action"
	DisplayStatusCompleted     ProjectDisplayStatus = "completed"
	DisplayStatusOnHold        ProjectDisplayStatus = "on_hold"
	DisplayStatusDispute       ProjectDisplayStatus = "dispute"
	DisplayStatusCancelled     ProjectDisplayStatus = "cancelled"
)

// StageDisplayStatus — вычисленный статус одной стадии.
type StageDisplayStatus string

const (
	StageNotStarted StageDisplayStatus = "not_started"
	StageActive     StageDisplayStatus = "active"
	StageCompleted  StageDisplayStatus = "completed"
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
	ID               uuid.UUID            `json:"id"`
	LeadID           *uuid.UUID           `json:"lead_id,omitempty"`
	SpecialistUserID *uuid.UUID           `json:"specialist_user_id,omitempty"`
	Title            string               `json:"title"`
	Status           ProjectStatus        `json:"status"`
	DisplayStatus    ProjectDisplayStatus `json:"display_status"`
	// Progress — % по весам видимых шагов (см. progress.go).
	Progress         float64    `json:"progress"`
	CurrentStepID    *uuid.UUID `json:"current_step_id,omitempty"`
	CurrentStepTitle *string    `json:"current_step_title,omitempty"`
	// CurrentStepCode — нужен фронту для step-descriptions[code].
	CurrentStepCode *string `json:"current_step_code,omitempty"`
	// CurrentStepOwner — чтобы фронт понимал «ждём вас» vs «команда работает».
	CurrentStepOwner *StepOwner `json:"current_step_owner,omitempty"`
	// CurrentStepStatus — чтобы решать показывать ли кнопки в шаге.
	CurrentStepStatus *StepStatus `json:"current_step_status,omitempty"`
	// RevisionsIncluded — сырое значение из БД (бэк-compat).
	RevisionsIncluded int `json:"revisions_included"`
	RevisionsUsed     int `json:"revisions_used"`
	// RevisionsTotal — синоним RevisionsIncluded по новой конвенции FE
	// (фикс §3.1 брифа). FE на новых компонентах использует именно его.
	RevisionsTotal int        `json:"revisions_total"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// Stages — funnel-дерево, встроенное в выдачу. На list и detail
	// отдаём одинаковую форму. Payload растёт, но убирает N+1 на фронте.
	Stages []StageView `json:"stages,omitempty"`
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
	ID            uuid.UUID          `json:"id"`
	Code          string             `json:"code"`
	Title         string             `json:"title"`
	SortOrder     int                `json:"sort_order"`
	DisplayStatus StageDisplayStatus `json:"display_status"`
	StepsTotal    int                `json:"steps_total"`
	StepsDone     int                `json:"steps_done"`
	StartedAt     *time.Time         `json:"started_at,omitempty"`
	CompletedAt   *time.Time         `json:"completed_at,omitempty"`
	Steps         []StepView         `json:"steps"`
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
	// IsCurrent — этот шаг сейчас считается «активным» по правилу
	// DeriveCurrentStep. Ровно один шаг в проекте может быть current=true.
	IsCurrent   bool       `json:"is_current"`
	ETADate     *time.Time `json:"eta_date,omitempty"`
	CTAPayload  []byte     `json:"cta_payload,omitempty"` // raw JSON
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
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
