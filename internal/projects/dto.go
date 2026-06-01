package projects

import (
	"time"

	"github.com/google/uuid"
)

// ---- enums ----

// ProjectStatus — общий жизненный цикл проекта (см. enum в БД).
type ProjectStatus string

const (
	ProjectStatusDraft     ProjectStatus = "draft"
	ProjectStatusActive    ProjectStatus = "active"
	ProjectStatusOnHold    ProjectStatus = "on_hold"
	ProjectStatusDone      ProjectStatus = "done"
	ProjectStatusCancelled ProjectStatus = "cancelled"
	ProjectStatusDispute   ProjectStatus = "dispute"
)

// StepStatus — стейт-машина шага: pending → in_progress → done|skipped,
// in_progress → waiting_client → done|rejected, rejected → in_progress.
type StepStatus string

const (
	StepStatusPending        StepStatus = "pending"
	StepStatusInProgress     StepStatus = "in_progress"
	StepStatusWaitingClient  StepStatus = "waiting_client"
	StepStatusDone           StepStatus = "done"
	StepStatusRejected       StepStatus = "rejected"
	StepStatusSkipped        StepStatus = "skipped"
)

// ProjectSource — откуда пришёл проект (для аналитики и UI-меток).
type ProjectSource string

const (
	SourceMarketplace      ProjectSource = "marketplace"
	SourceManual           ProjectSource = "manual"
	SourceReferral         ProjectSource = "referral"
	SourceReturningClient  ProjectSource = "returning_client"
)

// Owner шага. Дублируется из pipelines, но проект — самостоятельный домен.
const (
	OwnerClient = "client"
	OwnerTeam   = "team"
	OwnerSystem = "system"
)

// ---- display_status (computed) ----

// ProjectDisplayStatus — computed-статус для UI. Считается из status проекта
// и состояния шагов; описан в docs/CRM_V5_BRIEF.md §4.6.
type ProjectDisplayStatus string

const (
	ProjectDisplayNotStarted    ProjectDisplayStatus = "not_started"
	ProjectDisplayInProgress    ProjectDisplayStatus = "in_progress"
	ProjectDisplayWaitingAction ProjectDisplayStatus = "waiting_action"
	ProjectDisplayCompleted     ProjectDisplayStatus = "completed"
	ProjectDisplayOnHold        ProjectDisplayStatus = "on_hold"
	ProjectDisplayCancelled     ProjectDisplayStatus = "cancelled"
)

// StageDisplayStatus — computed-статус стадии в воронке.
type StageDisplayStatus string

const (
	StageDisplayNotStarted StageDisplayStatus = "not_started"
	StageDisplayActive     StageDisplayStatus = "active"
	StageDisplayCompleted  StageDisplayStatus = "completed"
)

// ---- entities ----

// Project — основа. Записи в БД. Без вложенных стадий/шагов.
type Project struct {
	ID                uuid.UUID  `json:"id"`
	LeadID            *uuid.UUID `json:"lead_id,omitempty"`
	LeadRecipientID   *uuid.UUID `json:"lead_recipient_id,omitempty"`
	ClientUserID      uuid.UUID  `json:"client_user_id"`
	SpecialistUserID  *uuid.UUID `json:"specialist_user_id,omitempty"`
	AssignedToUserID  *uuid.UUID `json:"assigned_to_user_id,omitempty"`
	PipelineID        uuid.UUID  `json:"pipeline_id"`
	Title             string     `json:"title"`
	Source            ProjectSource `json:"source"`
	Status            ProjectStatus `json:"status"`
	RevisionsIncluded int        `json:"revisions_included"`
	RevisionsUsed     int        `json:"revisions_used"`
	Budget            *int       `json:"budget,omitempty"`
	Notes             string     `json:"notes,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Stage — снэпшот стадии для конкретного проекта.
type Stage struct {
	ID          uuid.UUID  `json:"id"`
	ProjectID   uuid.UUID  `json:"project_id"`
	Name        string     `json:"name"`
	SortOrder   int        `json:"sort_order"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Step — снэпшот шага конкретного проекта. Status — стейт-машина.
type Step struct {
	ID                  uuid.UUID  `json:"id"`
	ProjectID           uuid.UUID  `json:"project_id"`
	StageID             uuid.UUID  `json:"stage_id"`
	Name                string     `json:"name"`
	Owner               string     `json:"owner"`
	Status              StepStatus `json:"status"`
	DurationDays        int        `json:"duration_days"`
	VisibleToClient     bool       `json:"visible_to_client"`
	VisibleToSpecialist bool       `json:"visible_to_specialist"`
	Weight              int        `json:"weight"`
	SortOrder           int        `json:"sort_order"`
	IsReview            bool       `json:"is_review"`
	EtaDate             *time.Time `json:"eta_date,omitempty"`
	ReviewDeadline      *time.Time `json:"review_deadline,omitempty"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// ---- views для клиента ----

// StepView — шаг как видит его клиент. Включает is_current (для подсветки)
// и описание. Скрытые шаги (visible_to_client=false) НЕ попадают в funnel.
type StepView struct {
	Step
	IsCurrent bool `json:"is_current"`
}

// StageView — стадия с display_status, прогрессом, видимыми шагами.
type StageView struct {
	Stage
	DisplayStatus StageDisplayStatus `json:"display_status"`
	StepsTotal    int                `json:"steps_total"`
	StepsDone     int                `json:"steps_done"`
	Steps         []StepView         `json:"steps"`
}

// ProjectClientView — полный ответ клиенту. Включает display_status, текущий
// шаг (для главного блока «что сейчас») и таймлайн стадий.
type ProjectClientView struct {
	Project
	DisplayStatus ProjectDisplayStatus `json:"display_status"`
	// Progress — взвешенный % выполнения по видимым клиенту шагам.
	Progress float64 `json:"progress"`
	// CurrentStep* — пришедший на «передовую» шаг (см. DeriveCurrentStep).
	// На UI идёт в hero-блок «Что сейчас». Может быть пустым (проект завершён).
	CurrentStepID     *uuid.UUID `json:"current_step_id,omitempty"`
	CurrentStepTitle  string     `json:"current_step_title,omitempty"`
	CurrentStepOwner  string     `json:"current_step_owner,omitempty"`
	CurrentStepStatus StepStatus `json:"current_step_status,omitempty"`
	// RevisionsTotal — алиас на RevisionsIncluded для UI («осталось X из Y»).
	RevisionsTotal int         `json:"revisions_total"`
	Stages         []StageView `json:"stages"`
}

// ---- inputs ----

// StartProjectInput — параметры запуска проекта со снэпшотом пайплайна.
// Если StartedAt = nil, сервис проставляет now() (нужен для расчёта eta_date).
type StartProjectInput struct {
	ClientUserID     uuid.UUID
	SpecialistUserID *uuid.UUID
	AssignedToUserID *uuid.UUID
	LeadID           *uuid.UUID
	LeadRecipientID  *uuid.UUID
	PipelineID       uuid.UUID
	Title            string
	Source           ProjectSource
	Budget           *int
	Notes            string
	StartedAt        *time.Time
}
