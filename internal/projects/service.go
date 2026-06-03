package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	// ErrRevisionsExhausted — клиент запросил правку, но лимит исчерпан.
	// Сервис всё равно переводит проект в dispute (см. TransitionStep) и
	// возвращает эту ошибку — фронт показывает соответствующее сообщение.
	ErrRevisionsExhausted = errors.New("revisions exhausted")
	// ErrNotClientStep — действие требует owner=client (approve/revision/review).
	ErrNotClientStep = errors.New("step is not assigned to client")
	// ErrNotReviewStep — submit_review для is_review=false.
	ErrNotReviewStep = errors.New("step is not a review step")
)

type Service struct {
	repo                     *Repo
	reviewDeadlineDuration   time.Duration
	defaultPipelineProvider  DefaultPipelineProvider
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// WithReviewDeadline — длительность дедлайна на review-шаг (по умолчанию 7 дней
// если не задано). Используется при переходе review-шага в waiting_client.
func (s *Service) WithReviewDeadline(d time.Duration) *Service {
	s.reviewDeadlineDuration = d
	return s
}

// reviewDeadline — длина дедлайна для review-шагов. Default 7 дней — чтобы
// сервис не падал, если WithReviewDeadline не вызван (в тестах удобно).
func (s *Service) reviewDeadline() time.Duration {
	if s.reviewDeadlineDuration > 0 {
		return s.reviewDeadlineDuration
	}
	return 7 * 24 * time.Hour
}

// StartProject — публичная обёртка над repo.StartProject с валидацией DTO.
func (s *Service) StartProject(ctx context.Context, in StartProjectInput) (uuid.UUID, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return uuid.Nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Title) > 200 {
		return uuid.Nil, fmt.Errorf("%w: title is too long", ErrInvalidInput)
	}
	if in.ClientUserID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: client_user_id is required", ErrInvalidInput)
	}
	if in.PipelineID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: pipeline_id is required", ErrInvalidInput)
	}
	if in.Source == "" {
		in.Source = SourceManual
	}
	return s.repo.StartProject(ctx, in)
}

// ---- Клиентские чтения ----

// ListClientProjects — список проектов клиента с enrich-ом display_status.
// SQL-стоимость: ListForClient + LoadStagesBatch + LoadStepsBatch + (опц.)
// LoadClientDisplayNames — 3..4 запроса независимо от количества проектов.
func (s *Service) ListClientProjects(ctx context.Context, clientID uuid.UUID) ([]ProjectClientView, error) {
	projects, err := s.repo.ListForClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return []ProjectClientView{}, nil
	}
	ids := make([]uuid.UUID, 0, len(projects))
	specSet := map[uuid.UUID]struct{}{}
	for _, p := range projects {
		ids = append(ids, p.ID)
		if p.SpecialistUserID != nil {
			specSet[*p.SpecialistUserID] = struct{}{}
		}
	}
	stagesByProject, err := s.repo.LoadStagesBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	stepsByProject, err := s.repo.LoadStepsBatch(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	if len(specSet) > 0 {
		specIDs := make([]uuid.UUID, 0, len(specSet))
		for id := range specSet {
			specIDs = append(specIDs, id)
		}
		// best-effort: имена специалистов нужны только для отображения.
		if n, err := s.repo.LoadClientDisplayNames(ctx, specIDs); err == nil {
			names = n
		}
	}
	views := make([]ProjectClientView, 0, len(projects))
	for _, p := range projects {
		view := buildClientView(p, stagesByProject[p.ID], stepsByProject[p.ID])
		if p.SpecialistUserID != nil {
			view.SpecialistDisplayName = names[*p.SpecialistUserID]
		}
		views = append(views, view)
	}
	return views, nil
}

// GetClientProject — полный проект клиента с воронкой.
func (s *Service) GetClientProject(ctx context.Context, projectID, clientID uuid.UUID) (ProjectClientView, error) {
	p, err := s.repo.GetByIDForClient(ctx, projectID, clientID)
	if err != nil {
		return ProjectClientView{}, err
	}
	return s.enrichClientView(ctx, p)
}

// enrichClientView — single-project путь (GetClientProject). Для списка
// см. ListClientProjects: там стадии/шаги грузятся одной пачкой через batch.
func (s *Service) enrichClientView(ctx context.Context, p Project) (ProjectClientView, error) {
	stages, err := s.repo.LoadStages(ctx, p.ID)
	if err != nil {
		return ProjectClientView{}, err
	}
	steps, err := s.repo.LoadSteps(ctx, p.ID, true)
	if err != nil {
		return ProjectClientView{}, err
	}
	view := buildClientView(p, stages, steps)
	if p.SpecialistUserID != nil {
		// best-effort: имя specialist'а из specialist_profiles
		if names, err := s.repo.LoadClientDisplayNames(ctx, []uuid.UUID{*p.SpecialistUserID}); err == nil {
			view.SpecialistDisplayName = names[*p.SpecialistUserID]
		}
	}
	return view, nil
}

// buildClientView — чистая сборка view из уже загруженных данных. Никаких
// SQL: одна и та же логика работает и для single-project (GetClientProject),
// и для каждого элемента батч-листинга (ListClientProjects).
func buildClientView(p Project, stages []Stage, steps []Step) ProjectClientView {
	byStage := map[uuid.UUID][]StepView{}
	flat := make([]StepView, 0, len(steps))
	for _, st := range steps {
		v := StepView{Step: st}
		byStage[st.StageID] = append(byStage[st.StageID], v)
		flat = append(flat, v)
	}
	current := DeriveCurrentStep(flat)
	stageViews := make([]StageView, 0, len(stages))
	for _, st := range stages {
		stepsInStage := byStage[st.ID]
		for i := range stepsInStage {
			if current != nil && stepsInStage[i].ID == current.ID {
				stepsInStage[i].IsCurrent = true
			}
		}
		ds, done, total := DeriveStageDisplayStatus(stepsInStage)
		stageViews = append(stageViews, StageView{
			Stage:         st,
			DisplayStatus: ds,
			StepsTotal:    total,
			StepsDone:     done,
			Steps:         stepsInStage,
		})
	}
	view := ProjectClientView{
		Project:        p,
		DisplayStatus:  DeriveProjectDisplayStatus(p.Status, flat),
		Progress:       DeriveProgress(flat),
		RevisionsTotal: p.RevisionsIncluded,
		Stages:         stageViews,
	}
	if current != nil {
		view.CurrentStepID = &current.ID
		view.CurrentStepTitle = current.Name
		view.CurrentStepOwner = current.Owner
		view.CurrentStepStatus = current.Status
	}
	return view
}

// ---- Клиентские действия ----

// Approve — клиент апрувит шаг (owner=client, status=waiting_client → done).
// Не активирует следующий шаг автоматически: следующий team-шаг стартует
// менеджером (Ф4). Это даёт точку контроля: «клиент одобрил, можно делать».
func (s *Service) Approve(ctx context.Context, projectID, stepID, actorID uuid.UUID) (Step, error) {
	step, err := s.repo.GetStep(ctx, projectID, stepID)
	if err != nil {
		return Step{}, err
	}
	if step.Owner != OwnerClient {
		return Step{}, ErrNotClientStep
	}
	if step.Status != StepStatusWaitingClient {
		return Step{}, ErrInvalidTransition
	}
	res, err := s.repo.TransitionStep(ctx, TransitionInput{
		ProjectID:   projectID,
		StepID:      stepID,
		From:        StepStatusWaitingClient,
		To:          StepStatusDone,
		ActorUserID: actorID,
		ActorType:   "human",
	})
	if err != nil {
		return Step{}, err
	}
	return res.Step, nil
}

// RequestRevision — клиент жмёт «правки» на client-шаге. Шаг → rejected,
// возвращается предыдущий team-шаг (если он есть) в in_progress, проект
// получает revisions_used+1. Если revisions_used превысит revisions_included →
// status=dispute + ErrRevisionsExhausted (фронт показывает диалог менеджеру).
func (s *Service) RequestRevision(ctx context.Context, projectID, stepID, actorID uuid.UUID, comment string) (Step, error) {
	step, err := s.repo.GetStep(ctx, projectID, stepID)
	if err != nil {
		return Step{}, err
	}
	if step.Owner != OwnerClient {
		return Step{}, ErrNotClientStep
	}
	if step.Status != StepStatusWaitingClient {
		return Step{}, ErrInvalidTransition
	}
	if step.IsReview {
		// На review-шаге правки не делаем — клиент должен либо submit_review
		// либо проигнорировать (тогда worker через 7 дней авто-skip'нет).
		return Step{}, fmt.Errorf("%w: request_revision on review step", ErrInvalidTransition)
	}

	// 1. Переводим клиентский шаг в rejected с инкрементом revisions_used.
	res, err := s.repo.TransitionStep(ctx, TransitionInput{
		ProjectID:          projectID,
		StepID:             stepID,
		From:               StepStatusWaitingClient,
		To:                 StepStatusRejected,
		ActorUserID:        actorID,
		ActorType:          "human",
		Comment:            strings.TrimSpace(comment),
		IncrementRevisions: true,
	})
	if err != nil {
		return Step{}, err
	}

	// 2. Возвращаем предыдущий team-шаг в работу (если есть).
	prev, err := s.repo.FindPrevTeamStep(ctx, projectID, stepID)
	if err != nil && !errors.Is(err, ErrStepNotFound) {
		return Step{}, err
	}
	if err == nil {
		// done → in_progress (правки)
		if _, terr := s.repo.TransitionStep(ctx, TransitionInput{
			ProjectID:   projectID,
			StepID:      prev.ID,
			From:        StepStatusDone,
			To:          StepStatusInProgress,
			ActorUserID: actorID,
			ActorType:   "system",
			Comment:     "Возврат на доработку",
		}); terr != nil {
			// возврат не сработал — оставляем rejected как есть; менеджер увидит в кабинете.
			// не считаем критичной ошибкой.
			_ = terr
		}
	}

	if res.Disputed {
		return res.Step, ErrRevisionsExhausted
	}
	return res.Step, nil
}

// SubmitReview — клиент оставил отзыв на review-шаге (waiting_client →
// done). Сам объект reviews создаётся отдельным сервисом reviews; здесь
// фиксируем только шаг.
//
// actorID — id залогиненного клиента. Сервис сам проверяет, что проект
// принадлежит ему, чтобы ручка не была единственной точкой авторизации:
// если SubmitReview переиспользуется из другого контекста (manager-flow,
// internal job), доступ всё равно отвалится.
func (s *Service) SubmitReview(ctx context.Context, projectID, stepID, actorID uuid.UUID) (Step, error) {
	// Anti-enumeration: «чужой проект» неотличим от «нет такого» (404).
	if _, err := s.repo.GetByIDForClient(ctx, projectID, actorID); err != nil {
		return Step{}, err
	}
	step, err := s.repo.GetStep(ctx, projectID, stepID)
	if err != nil {
		return Step{}, err
	}
	if step.Owner != OwnerClient {
		return Step{}, ErrNotClientStep
	}
	if !step.IsReview {
		return Step{}, ErrNotReviewStep
	}
	if step.Status != StepStatusWaitingClient {
		return Step{}, ErrInvalidTransition
	}
	res, err := s.repo.TransitionStep(ctx, TransitionInput{
		ProjectID:   projectID,
		StepID:      stepID,
		From:        StepStatusWaitingClient,
		To:          StepStatusDone,
		ActorUserID: actorID,
		ActorType:   "human",
	})
	if err != nil {
		return Step{}, err
	}
	return res.Step, nil
}
