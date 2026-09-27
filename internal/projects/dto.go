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
	StepStatusPending       StepStatus = "pending"
	StepStatusInProgress    StepStatus = "in_progress"
	StepStatusWaitingClient StepStatus = "waiting_client"
	StepStatusDone          StepStatus = "done"
	StepStatusRejected      StepStatus = "rejected"
	StepStatusSkipped       StepStatus = "skipped"
)

// ProjectSource — откуда пришёл проект (для аналитики и UI-меток).
type ProjectSource string

const (
	SourceMarketplace     ProjectSource = "marketplace"
	SourceManual          ProjectSource = "manual"
	SourceReferral        ProjectSource = "referral"
	SourceReturningClient ProjectSource = "returning_client"
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

// ProjectKind — вид проекта (enum project_kind в 00032). Определяет, что
// у проекта вообще есть: воронка, выкладки или один срок и сдача.
type ProjectKind string

const (
	// KindCreatorsTurnkey — креаторы под ключ: выкладки, пять площадок,
	// ежедневная статистика.
	KindCreatorsTurnkey ProjectKind = "creators_turnkey"
	// KindBrandTurnkey — бренд под ключ: те же выкладки и та же
	// статистика, но без людей — ролики выходят с аккаунтов бренда.
	// Состава, проверки, чек-листов и начислений у него нет.
	KindBrandTurnkey ProjectKind = "brand_turnkey"
	// KindProductionTurnkey — продакшн под ключ: воронка pipelines.
	KindProductionTurnkey ProjectKind = "production_turnkey"
	// KindGeneral — общий проект: исполнитель и один срок.
	KindGeneral ProjectKind = "general"
)

// ---- entities ----

// Project — основа. Записи в БД. Без вложенных стадий/шагов.
type Project struct {
	ID                        uuid.UUID  `json:"id"`
	LeadID                    *uuid.UUID `json:"lead_id,omitempty"`
	LeadRecipientSpecialistID *uuid.UUID `json:"lead_recipient_specialist_id,omitempty"`
	// ClientUserID — клиент с аккаунтом. nil если проект заведён менеджером/
	// админом для клиента без регистрации; тогда контакты в ClientName/
	// ClientContact. CHECK constraint гарантирует одно из двух.
	ClientUserID *uuid.UUID `json:"client_user_id,omitempty"`
	// ClientName / ClientContact — заполняется когда client_user_id=NULL.
	// Для зарегистрированных клиентов остаются пустыми (берём из
	// client_profiles по client_user_id).
	ClientName       string     `json:"client_name,omitempty"`
	ClientContact    string     `json:"client_contact,omitempty"`
	SpecialistUserID *uuid.UUID `json:"specialist_user_id,omitempty"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id,omitempty"`
	// PipelineID — воронка. nil у общего проекта: воронки у него нет
	// вовсе, и отдавать нулевой uuid значило бы врать, что она есть.
	PipelineID *uuid.UUID `json:"pipeline_id,omitempty"`
	// Kind — вид проекта. Без него фронт отличал проект с креаторами от
	// воронки по наличию выкладок, то есть догадкой.
	Kind ProjectKind `json:"kind"`
	// IsTest — проект заведён для проверки стенда. Админский список такие
	// прячет по умолчанию: иначе половина строк в нём — «тест т8т 1234».
	IsTest            bool          `json:"is_test"`
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
	// SpecialistDisplayName — имя исполнителя (если назначен). Пусто если
	// specialist_user_id=nil или у юзера нет specialist_profile.
	SpecialistDisplayName string `json:"specialist_display_name,omitempty"`
	// SpecialistPrimaryCategory — title основной категории исполнителя
	// («Видеооператор», «Дизайнер»). Опознавательный знак карточки
	// проекта когда у клиента их несколько.
	SpecialistPrimaryCategory string `json:"specialist_primary_category,omitempty"`
	// ManagerDisplayName — имя менеджера проекта. Заказчик пишет не «в
	// поддержку», а конкретному человеку: у вкладки переписки в кабинете
	// стоит его имя, и брать это имя больше неоткуда. Пусто, пока
	// менеджер не назначен.
	ManagerDisplayName string               `json:"manager_display_name,omitempty"`
	DisplayStatus      ProjectDisplayStatus `json:"display_status"`
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
//
// Инвариант: должно быть задано одно из двух:
//   - ClientUserID (зарегистрированный клиент);
//   - ClientName + ClientContact (no-account клиент: менеджер/админ
//     ведёт проект для контакта, который не зарегистрировался).
//
// Service.StartProject валидирует это; миграция 00016 поддерживает
// то же на DB-level через CHECK constraint.
type StartProjectInput struct {
	ClientUserID              *uuid.UUID
	ClientName                string
	ClientContact             string
	SpecialistUserID          *uuid.UUID
	AssignedToUserID          *uuid.UUID
	LeadID                    *uuid.UUID
	LeadRecipientSpecialistID *uuid.UUID
	// PipelineID — воронка. uuid.Nil означает «воронки нет»: она бывает
	// только у продакшна. У креаторов вместо неё выкладки, у общего
	// проекта — один срок.
	PipelineID uuid.UUID
	// Kind — вид проекта. Пусто = production_turnkey: все проекты до
	// появления видов создавались из воронки и ведутся менеджером.
	Kind ProjectKind
	// IsTest — см. Project.IsTest. Ставится только при создании руками:
	// проекты, пришедшие из лида, тестовыми не бывают.
	IsTest    bool
	Title     string
	Source    ProjectSource
	Budget    *int
	Notes     string
	StartedAt *time.Time
}
