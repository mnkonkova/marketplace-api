package projects

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// AggregateProject — все события CRM-проекта помечаются этим
// aggregate-name'ом. Webhook-диспатчер в Фазе 6 фильтрует по нему.
const AggregateProject = "project"

// Event types. Соответствуют брифу §7.1.
const (
	EventProjectCreated        = "project.created"
	EventProjectCompleted      = "project.completed"
	EventProjectDisputed       = "project.disputed"
	EventStepTransitioned      = "step.transitioned"
	EventRevisionRequested     = "revision.requested"
	EventClientInviteGenerated = "client_invite.generated"
)

// ProjectEventPayload — единый формат полезной нагрузки для всех
// project-событий. Не все поля заполняются для каждого типа: для
// project.created заполнены client/specialist/manager/source/title,
// step-поля пусты; для step.transitioned заполнены step-поля + from/to.
//
// Поля денормализованы (email/name копируются из users в payload), чтобы
// n8n в обработчике не должен делать обратные запросы в БД. Цена —
// payload «застывает» в outbox: если юзер сменит email после события,
// в нотификации останется старый. Это сознательный trade-off (бриф §7).
type ProjectEventPayload struct {
	ProjectID     uuid.UUID     `json:"project_id"`
	ProjectSource ProjectSource `json:"project_source"`
	LeadID        *uuid.UUID    `json:"lead_id,omitempty"`
	Title         string        `json:"title"`

	// Step-context (только для step.transitioned / revision.requested).
	StepID     *uuid.UUID `json:"step_id,omitempty"`
	StepCode   string     `json:"step_code,omitempty"`
	StepOwner  StepOwner  `json:"step_owner,omitempty"`
	FromStatus StepStatus `json:"from,omitempty"`
	ToStatus   StepStatus `json:"to,omitempty"`

	// Actor — кто совершил действие. ActorType = human/service/system.
	ActorUserID *uuid.UUID `json:"actor_user_id,omitempty"`
	ActorType   string     `json:"actor_type,omitempty"`

	// Денормализованные «адресные» поля. Заполняются service-слоем при
	// emit'е. Если у юзера нет email — поле пустое (n8n маршрутизирует
	// по наличию).
	ClientUserID     uuid.UUID  `json:"client_user_id"`
	ClientEmail      string     `json:"client_email,omitempty"`
	ClientName       string     `json:"client_name,omitempty"`
	SpecialistUserID *uuid.UUID `json:"specialist_user_id,omitempty"`
	SpecialistEmail  string     `json:"specialist_email,omitempty"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id,omitempty"`
	ManagerEmail     string     `json:"manager_email,omitempty"`

	OccurredAt time.Time `json:"occurred_at"`
}

// emitProjectEvent — общий хелпер, чтобы все события писались
// единообразно: aggregate=project, aggregate_id=project_id (строка),
// payload — выше структура.
func emitProjectEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	payload ProjectEventPayload,
) error {
	return outbox.Emit(ctx, tx, AggregateProject, payload.ProjectID.String(), eventType, payload)
}
