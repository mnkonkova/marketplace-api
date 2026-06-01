package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// DefaultPipelineProvider — для интеграций (leads): даёт id текущей
// default-воронки. Реализуется pipelines.Service.GetDefaultPipelineID.
// Если default не настроен — ErrDefaultPipelineMissing.
type DefaultPipelineProvider interface {
	GetDefaultPipelineID(ctx context.Context) (uuid.UUID, error)
}

var ErrDefaultPipelineMissing = errors.New("no default pipeline configured")

// WithDefaultPipeline — подключает резолвер дефолтной воронки. Без него
// StartFromLead вернёт ErrDefaultPipelineMissing.
func (s *Service) WithDefaultPipeline(p DefaultPipelineProvider) *Service {
	s.defaultPipelineProvider = p
	return s
}

// StartFromLead — реализует leads.ProjectStarter. Создаёт проект с
// default-воронкой, client_user_id=clientID, assigned_to_user_id=NULL
// (в inbox менеджеров). lead_id связывается с leads.id для возможности
// потом отправить контакты обратно. title подтягивается из брифа.
// proposedSpecialistID — если клиент в брифе выбрал ровно одного спеца,
// мы кладём его в lead_recipient_specialist_id как «предложенного»; менеджер
// аппрувит через POST /manager/projects/{id}/approve_specialist.
// Если получателей несколько/ноль — оставляем NULL.
func (s *Service) StartFromLead(ctx context.Context, clientID uuid.UUID, leadID uuid.UUID, title, brief string, proposedSpecialistID *uuid.UUID) (uuid.UUID, error) {
	if s.defaultPipelineProvider == nil {
		return uuid.Nil, ErrDefaultPipelineMissing
	}
	pipelineID, err := s.defaultPipelineProvider.GetDefaultPipelineID(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve default pipeline: %w", err)
	}
	notes := brief // полный бриф попадает в notes, чтобы менеджер сразу видел
	return s.StartProject(ctx, StartProjectInput{
		ClientUserID:               clientID,
		LeadID:                     &leadID,
		LeadRecipientSpecialistID:  proposedSpecialistID,
		PipelineID:                 pipelineID,
		Title:                      title,
		Source:                     SourceMarketplace,
		Notes:                      notes,
	})
}
