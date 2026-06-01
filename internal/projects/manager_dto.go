package projects

import (
	"time"

	"github.com/google/uuid"
)

// PartyContact — контакты одного участника проекта (клиента или спеца).
// Все поля опциональные: показываем только заполненное. email берём из
// users.email, phone/telegram — из client_profiles либо specialist_profiles
// (см. manager_repo.LoadProjectParties).
type PartyContact struct {
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Telegram    string `json:"telegram,omitempty"`
}

// ProjectManagerView — карточка проекта для канбана менеджера. Кроме самого
// Project, тащит: текущую стадию (для расчёта колонки канбана), display_status,
// прогресс и кого ждём. Намеренно без полного дерева шагов — лёгкая выдача.
type ProjectManagerView struct {
	Project
	ClientDisplayName  string               `json:"client_display_name,omitempty"`
	Client             *PartyContact        `json:"client,omitempty"`
	Specialist         *PartyContact        `json:"specialist,omitempty"`
	DisplayStatus      ProjectDisplayStatus `json:"display_status"`
	Progress           float64              `json:"progress"`

	// CurrentStageID/Title — куда положить карточку в канбане.
	CurrentStageID    *uuid.UUID `json:"current_stage_id,omitempty"`
	CurrentStageName  string     `json:"current_stage_name,omitempty"`
	CurrentStageOrder int        `json:"current_stage_order"`

	CurrentStepID     *uuid.UUID `json:"current_step_id,omitempty"`
	CurrentStepTitle  string     `json:"current_step_title,omitempty"`
	CurrentStepOwner  string     `json:"current_step_owner,omitempty"`
	CurrentStepStatus StepStatus `json:"current_step_status,omitempty"`
}

// ProjectFullView — детальная страница проекта в кабинете менеджера/админа.
// Включает все шаги (включая скрытые от клиента), стадии с их display_status,
// и события (в Ф7 — лента активности).
type ProjectFullView struct {
	Project
	Client            *PartyContact        `json:"client,omitempty"`
	Specialist        *PartyContact        `json:"specialist,omitempty"`
	// ProposedSpecialist — кого клиент выбрал в брифе (lead_recipient_specialist_id);
	// пока не подтверждён менеджером (Specialist=nil). Фронт показывает блок
	// «Клиент выбрал …» c кнопками Approve/Reject.
	ProposedSpecialist *PartyContact        `json:"proposed_specialist,omitempty"`
	DisplayStatus     ProjectDisplayStatus `json:"display_status"`
	Progress          float64              `json:"progress"`
	CurrentStepID     *uuid.UUID           `json:"current_step_id,omitempty"`
	CurrentStepTitle  string               `json:"current_step_title,omitempty"`
	CurrentStepOwner  string               `json:"current_step_owner,omitempty"`
	CurrentStepStatus StepStatus           `json:"current_step_status,omitempty"`
	Stages            []StageView          `json:"stages"`
}

// ManagerPatchInput — поля, которые менеджер правит inline на карточке.
// UpdatedAt — optimistic-lock версия projects.updated_at.
type ManagerPatchInput struct {
	Title     *string    `json:"title,omitempty"`
	Budget    *int       `json:"budget,omitempty"`
	Notes     *string    `json:"notes,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}
