package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---- Менеджерские чтения ----

// ListInbox — проекты без assigned_to, со enrich-ом (display_status, прогресс,
// текущий шаг). Все enrich'и идут N+1 — для MVP объёма это ОК.
func (s *Service) ListInbox(ctx context.Context) ([]ProjectManagerView, error) {
	projects, err := s.repo.ListInbox(ctx)
	if err != nil {
		return nil, err
	}
	return s.enrichManagerViews(ctx, projects)
}

// ListAssignedTo — мои проекты (для канбана).
func (s *Service) ListAssignedTo(ctx context.Context, managerID uuid.UUID) ([]ProjectManagerView, error) {
	projects, err := s.repo.ListAssignedTo(ctx, managerID)
	if err != nil {
		return nil, err
	}
	return s.enrichManagerViews(ctx, projects)
}

// ListAll — все проекты (админ-кабинет, включая канбан со всеми).
func (s *Service) ListAll(ctx context.Context, statusFilter string) ([]ProjectManagerView, error) {
	projects, err := s.repo.ListAll(ctx, statusFilter)
	if err != nil {
		return nil, err
	}
	return s.enrichManagerViews(ctx, projects)
}

func (s *Service) enrichManagerViews(ctx context.Context, projects []Project) ([]ProjectManagerView, error) {
	views := make([]ProjectManagerView, 0, len(projects))
	clientIDs := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		clientIDs = append(clientIDs, p.ClientUserID)
	}
	names, err := s.repo.LoadClientDisplayNames(ctx, clientIDs)
	if err != nil {
		// best-effort: имена клиентов не критичны
		names = map[uuid.UUID]string{}
	}
	for _, p := range projects {
		view := ProjectManagerView{
			Project:           p,
			ClientDisplayName: names[p.ClientUserID],
		}
		// все шаги (включая скрытые) — менеджер видит всё.
		steps, err := s.repo.LoadSteps(ctx, p.ID, false)
		if err != nil {
			return nil, err
		}
		stepViews := make([]StepView, 0, len(steps))
		for _, st := range steps {
			stepViews = append(stepViews, StepView{Step: st})
		}
		view.DisplayStatus = DeriveProjectDisplayStatus(p.Status, stepViews)
		view.Progress = DeriveProgress(stepViews)
		if cur := DeriveCurrentStep(stepViews); cur != nil {
			view.CurrentStepID = &cur.ID
			view.CurrentStepTitle = cur.Name
			view.CurrentStepOwner = cur.Owner
			view.CurrentStepStatus = cur.Status
		}
		// Текущая стадия = первая с шагом не в done|skipped.
		curStage, _, err := s.repo.CurrentAndNextStage(ctx, p.ID)
		if err == nil && curStage != nil {
			view.CurrentStageID = &curStage.ID
			view.CurrentStageName = curStage.Name
			view.CurrentStageOrder = curStage.SortOrder
		}
		views = append(views, view)
	}
	return views, nil
}

// GetFull — детальная страница проекта (менеджер или админ).
// Если managerID=uuid.Nil — без проверки assigned_to (для админа).
func (s *Service) GetFull(ctx context.Context, projectID, managerID uuid.UUID) (ProjectFullView, error) {
	p, err := s.repo.GetByIDForManager(ctx, projectID, managerID)
	if err != nil {
		return ProjectFullView{}, err
	}
	stages, err := s.repo.LoadStages(ctx, projectID)
	if err != nil {
		return ProjectFullView{}, err
	}
	steps, err := s.repo.LoadSteps(ctx, projectID, false)
	if err != nil {
		return ProjectFullView{}, err
	}

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
		ss := byStage[st.ID]
		for i := range ss {
			if current != nil && ss[i].ID == current.ID {
				ss[i].IsCurrent = true
			}
		}
		ds, done, total := DeriveStageDisplayStatus(ss)
		stageViews = append(stageViews, StageView{
			Stage: st, DisplayStatus: ds, StepsTotal: total, StepsDone: done, Steps: ss,
		})
	}
	out := ProjectFullView{
		Project:       p,
		DisplayStatus: DeriveProjectDisplayStatus(p.Status, flat),
		Progress:      DeriveProgress(flat),
		Stages:        stageViews,
	}
	if current != nil {
		out.CurrentStepID = &current.ID
		out.CurrentStepTitle = current.Name
		out.CurrentStepOwner = current.Owner
		out.CurrentStepStatus = current.Status
	}
	return out, nil
}

// ---- Менеджерские действия ----

func (s *Service) Claim(ctx context.Context, projectID, managerID uuid.UUID) error {
	return s.repo.Claim(ctx, projectID, managerID)
}

func (s *Service) AdvanceStage(ctx context.Context, projectID, actorID uuid.UUID) (Project, error) {
	return s.repo.AdvanceStage(ctx, projectID, actorID, s.reviewDeadline())
}

// StartStep — менеджер активирует pending-шаг. owner=client → waiting_client
// (мяч у клиента); team/system → in_progress. Для review-шага ставит
// deadline = now + reviewDeadline (worker auto-skip-нет по истечении).
func (s *Service) StartStep(ctx context.Context, projectID, stepID, actorID uuid.UUID) (Step, error) {
	step, err := s.repo.GetStep(ctx, projectID, stepID)
	if err != nil {
		return Step{}, err
	}
	if step.Status != StepStatusPending {
		return Step{}, ErrInvalidTransition
	}
	to := StepStatusInProgress
	if step.Owner == OwnerClient {
		to = StepStatusWaitingClient
	}
	in := TransitionInput{
		ProjectID:   projectID,
		StepID:      stepID,
		From:        StepStatusPending,
		To:          to,
		ActorUserID: actorID,
		ActorType:   "human",
	}
	if step.IsReview && to == StepStatusWaitingClient {
		deadline := time.Now().Add(s.reviewDeadline())
		in.SetReviewDeadline = &deadline
	}
	res, err := s.repo.TransitionStep(ctx, in)
	if err != nil {
		return Step{}, err
	}
	return res.Step, nil
}

// CompleteStep — менеджер закрывает team/system-шаг. Не используем на
// client-шагах (клиент сам апрувит). Из in_progress → done.
func (s *Service) CompleteStep(ctx context.Context, projectID, stepID, actorID uuid.UUID) (Step, error) {
	step, err := s.repo.GetStep(ctx, projectID, stepID)
	if err != nil {
		return Step{}, err
	}
	if step.Owner == OwnerClient {
		return Step{}, fmt.Errorf("%w: client step is closed by client action", ErrInvalidTransition)
	}
	if step.Status != StepStatusInProgress {
		return Step{}, ErrInvalidTransition
	}
	res, err := s.repo.TransitionStep(ctx, TransitionInput{
		ProjectID:   projectID,
		StepID:      stepID,
		From:        StepStatusInProgress,
		To:          StepStatusDone,
		ActorUserID: actorID,
		ActorType:   "human",
	})
	if err != nil {
		return Step{}, err
	}
	return res.Step, nil
}

// SkipStep — менеджер пропускает шаг (например, согласовано с клиентом
// очно — нет смысла ждать апрува). Комментарий обязателен.
func (s *Service) SkipStep(ctx context.Context, projectID, stepID, actorID uuid.UUID, comment string) (Step, error) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return Step{}, fmt.Errorf("%w: comment is required for skip", ErrInvalidInput)
	}
	step, err := s.repo.GetStep(ctx, projectID, stepID)
	if err != nil {
		return Step{}, err
	}
	if step.Status == StepStatusDone || step.Status == StepStatusSkipped {
		return Step{}, ErrInvalidTransition
	}
	res, err := s.repo.TransitionStep(ctx, TransitionInput{
		ProjectID:   projectID,
		StepID:      stepID,
		From:        step.Status,
		To:          StepStatusSkipped,
		ActorUserID: actorID,
		ActorType:   "human",
		Comment:     comment,
	})
	if err != nil {
		return Step{}, err
	}
	return res.Step, nil
}

// PatchProject — title/budget/notes с optimistic-lock.
func (s *Service) PatchProject(ctx context.Context, projectID uuid.UUID, in ManagerPatchInput) (Project, error) {
	if in.Title != nil {
		if v := strings.TrimSpace(*in.Title); v == "" {
			return Project{}, fmt.Errorf("%w: title cannot be empty", ErrInvalidInput)
		}
	}
	if in.Budget != nil && *in.Budget < 0 {
		return Project{}, fmt.Errorf("%w: budget must be >= 0", ErrInvalidInput)
	}
	return s.repo.PatchProject(ctx, projectID, in)
}

// AssertManagerHasAccess — проверяет, что переданный пользователь является
// ответственным менеджером проекта. Используется как первичная проверка
// в каждом handler-action (чтобы не возиться с RequireRoles на каждом
// конкретном проекте). Админ — отдельный путь через GetFull(uuid.Nil).
func (s *Service) AssertManagerHasAccess(ctx context.Context, projectID, managerID uuid.UUID) error {
	if managerID == uuid.Nil {
		return errors.New("manager id required")
	}
	_, err := s.repo.GetByIDForManager(ctx, projectID, managerID)
	return err
}
