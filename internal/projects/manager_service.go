package projects

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// ListAll — страница админского списка (он же источник для канбана со
// всеми проектами). Фильтры и пагинация считаются в SQL, а не после
// выборки: enrich стоит нескольких запросов на страницу, и делать его для
// строк, которые потом отбросят, незачем.
func (s *Service) ListAll(ctx context.Context, p AdminListParams) (AdminListResult, error) {
	projects, total, err := s.repo.ListAll(ctx, p)
	if err != nil {
		// Отказы по параметрам repo помечает ErrInvalidInput сам — здесь
		// достаточно пропустить ошибку наверх. Раньше на её месте стоял
		// разбор текста по префиксу "invalid ", и первая же правка
		// формулировки молча превращала 400 в 500.
		return AdminListResult{}, err
	}
	views, err := s.enrichManagerViews(ctx, projects)
	if err != nil {
		return AdminListResult{}, err
	}
	limit := p.Limit
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	return AdminListResult{Items: views, Total: total, Limit: limit, Offset: offset}, nil
}

// ListBySpecialist — проекты специалиста (read-only вкладка кабинета).
// Enrich тот же что и у менеджера — display_status/progress/current_*.
func (s *Service) ListBySpecialist(ctx context.Context, specialistID uuid.UUID) ([]ProjectManagerView, error) {
	projects, err := s.repo.ListBySpecialist(ctx, specialistID)
	if err != nil {
		return nil, err
	}
	return s.enrichManagerViews(ctx, projects)
}

func (s *Service) enrichManagerViews(ctx context.Context, projects []Project) ([]ProjectManagerView, error) {
	views := make([]ProjectManagerView, 0, len(projects))
	if len(projects) == 0 {
		return views, nil
	}
	// Собираем уникальные user_id всех участников — для одного батч-запроса.
	// No-account клиенты (ClientUserID=nil) пропускаются: для них
	// view.Client заполняем из p.ClientName/p.ClientContact ниже.
	idSet := make(map[uuid.UUID]struct{}, 2*len(projects))
	projectIDs := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		projectIDs = append(projectIDs, p.ID)
		if p.ClientUserID != nil {
			idSet[*p.ClientUserID] = struct{}{}
		}
		if p.SpecialistUserID != nil {
			idSet[*p.SpecialistUserID] = struct{}{}
		}
		// Менеджера тащим тем же батчем: у него нет профиля, и
		// LoadPartyContacts отдаст ему имя из почты — этого для колонки
		// «Менеджер» достаточно.
		if p.AssignedToUserID != nil {
			idSet[*p.AssignedToUserID] = struct{}{}
		}
	}
	ids := make([]uuid.UUID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	contacts, err := s.repo.LoadPartyContacts(ctx, ids)
	if err != nil {
		// best-effort: контакты не критичны для канбана
		contacts = map[uuid.UUID]PartyContact{}
	}
	// У проекта с креаторами шагов нет, и прогресс по ним всегда ноль.
	// Его меру считаем по выкладкам — тем же запросом на весь список.
	// Берём сразу парой чисел: процент из них выводится, обратно — нет.
	pubCounts, err := s.repo.PublicationCounts(ctx, turnkeyIDs(projects))
	if err != nil {
		pubCounts = map[uuid.UUID]PublicationCount{}
	}
	// Стадии и шаги — батчем, без N+1 на список (было: 2*N запросов в канбане).
	stagesByProject, err := s.repo.LoadStagesBatch(ctx, projectIDs)
	if err != nil {
		return nil, err
	}
	stepsByProject, err := s.repo.LoadStepsBatch(ctx, projectIDs, false)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		view := ProjectManagerView{Project: p}
		if p.ClientUserID != nil {
			if c, ok := contacts[*p.ClientUserID]; ok {
				cc := c
				view.Client = &cc
				view.ClientDisplayName = cc.DisplayName
			}
		} else if p.ClientName != "" {
			// No-account клиент: контакты прямо на проекте.
			cc := PartyContact{
				DisplayName: p.ClientName,
				Phone:       p.ClientContact,
			}
			view.Client = &cc
			view.ClientDisplayName = p.ClientName
		}
		if p.SpecialistUserID != nil {
			if c, ok := contacts[*p.SpecialistUserID]; ok {
				cc := c
				view.Specialist = &cc
			}
		}
		if p.AssignedToUserID != nil {
			view.ManagerDisplayName = contacts[*p.AssignedToUserID].DisplayName
		}
		steps := stepsByProject[p.ID]
		stepViews := make([]StepView, 0, len(steps))
		for _, st := range steps {
			stepViews = append(stepViews, StepView{Step: st})
		}
		view.DisplayStatus = DeriveProjectDisplayStatus(p.Status, stepViews)
		view.Progress = DeriveProgress(stepViews)
		// Шаги меряем сделанными и пропущенными: пропущенный шаг проекту
		// больше ничего не должен, и держать его в «осталось» — врать.
		if len(stepViews) > 0 {
			done := 0
			for _, sv := range stepViews {
				if sv.Status == StepStatusDone || sv.Status == StepStatusSkipped {
					done++
				}
			}
			view.ProgressDone, view.ProgressTotal = done, len(stepViews)
			view.ProgressUnit = ProgressUnitSteps
		}
		if c, ok := pubCounts[p.ID]; ok && c.Total > 0 {
			view.Progress = float64(c.Done) / float64(c.Total) * 100
			view.ProgressDone, view.ProgressTotal = c.Done, c.Total
			view.ProgressUnit = ProgressUnitPublications
		}
		if cur := DeriveCurrentStep(stepViews); cur != nil {
			view.CurrentStepID = &cur.ID
			view.CurrentStepTitle = cur.Name
			view.CurrentStepOwner = cur.Owner
			view.CurrentStepStatus = cur.Status
		}
		// Текущая стадия = первая с шагом не в done|skipped. Раньше — отдельный
		// SQL на каждый проект; теперь считаем из уже загруженных стадий+шагов.
		if cur := deriveCurrentStage(stagesByProject[p.ID], steps); cur != nil {
			view.CurrentStageID = &cur.ID
			view.CurrentStageName = cur.Name
			view.CurrentStageOrder = cur.SortOrder
		}
		views = append(views, view)
	}
	return views, nil
}

// deriveCurrentStage — текущая активная стадия из in-memory стадий+шагов:
// первая (по sort_order) стадия, в которой есть хотя бы один шаг НЕ в
// done|skipped. Если все стадии завершены — nil. Эквивалентно
// CurrentAndNextStage без SQL — для использования в батч-листинге.
// stages должны быть отсортированы по sort_order (LoadStagesBatch это делает).
func deriveCurrentStage(stages []Stage, steps []Step) *Stage {
	stepsByStage := map[uuid.UUID][]Step{}
	for _, st := range steps {
		stepsByStage[st.StageID] = append(stepsByStage[st.StageID], st)
	}
	for i := range stages {
		ss := stepsByStage[stages[i].ID]
		if len(ss) == 0 {
			// Стадия без шагов считаем not-done (так же как CurrentAndNextStage).
			return &stages[i]
		}
		allDone := true
		for _, s := range ss {
			if s.Status != StepStatusDone && s.Status != StepStatusSkipped {
				allDone = false
				break
			}
		}
		if !allDone {
			return &stages[i]
		}
	}
	return nil
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
	if pct, ok := s.turnkeyProgress(ctx, p); ok {
		out.Progress = pct
	}
	// Контакты обеих сторон — для блока «Связаться» на странице проекта.
	// No-account клиент (ClientUserID=nil): берём ClientName/ClientContact
	// прямо с проекта, без обращения к users.
	ids := make([]uuid.UUID, 0, 3)
	if p.ClientUserID != nil {
		ids = append(ids, *p.ClientUserID)
	}
	if p.SpecialistUserID != nil {
		ids = append(ids, *p.SpecialistUserID)
	}
	if p.LeadRecipientSpecialistID != nil && p.SpecialistUserID == nil {
		ids = append(ids, *p.LeadRecipientSpecialistID)
	}
	if contacts, cerr := s.repo.LoadPartyContacts(ctx, ids); cerr == nil {
		if p.ClientUserID != nil {
			if c, ok := contacts[*p.ClientUserID]; ok {
				cc := c
				out.Client = &cc
			}
		} else if p.ClientName != "" {
			cc := PartyContact{DisplayName: p.ClientName, Phone: p.ClientContact}
			out.Client = &cc
		}
		if p.SpecialistUserID != nil {
			if c, ok := contacts[*p.SpecialistUserID]; ok {
				cc := c
				out.Specialist = &cc
			}
		} else if p.LeadRecipientSpecialistID != nil {
			if c, ok := contacts[*p.LeadRecipientSpecialistID]; ok {
				cc := c
				out.ProposedSpecialist = &cc
			}
		}
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

// ApproveProposedSpecialist — менеджер/админ подтверждает выбор клиента.
func (s *Service) ApproveProposedSpecialist(ctx context.Context, projectID, actorID uuid.UUID) (uuid.UUID, error) {
	return s.repo.ApproveProposedSpecialist(ctx, projectID, actorID)
}

// AssignSpecialist — назначает конкретного спеца на проект напрямую (минуя
// proposed-flow). Используется в Phase 1 (manual-проекты без бриф'a) и
// после реджекта предложенного спеца.
func (s *Service) AssignSpecialist(ctx context.Context, projectID, actorID, specialistID uuid.UUID) error {
	return s.repo.AssignSpecialist(ctx, projectID, actorID, specialistID)
}

// RejectProposedSpecialist — менеджер/админ отклоняет предложенного клиентом.
func (s *Service) RejectProposedSpecialist(ctx context.Context, projectID, actorID uuid.UUID, reason string) error {
	return s.repo.RejectProposedSpecialist(ctx, projectID, actorID, reason)
}

// CancelProject — админская «удалить проект»: status='cancelled', без
// удаления строки. Обратима ручкой возврата (см. RestoreProject).
func (s *Service) CancelProject(ctx context.Context, projectID, actorID uuid.UUID, reason string) error {
	return s.repo.CancelProject(ctx, projectID, actorID, reason)
}

// AssignManager — админская операция (POST /admin/projects/{id}/assign).
// managerID=nil → unassign.
func (s *Service) AssignManager(ctx context.Context, projectID uuid.UUID, managerID *uuid.UUID, actorID uuid.UUID) error {
	return s.repo.AssignManager(ctx, projectID, managerID, actorID)
}

// AdvanceStage / MoveProjectToStage принимают optional expectedUpdatedAt
// для optimistic-lock — фронт шлёт значение из последнего GET. Без него
// (nil) лок не активируется (back-compat).
func (s *Service) AdvanceStage(ctx context.Context, projectID, actorID uuid.UUID, expectedUpdatedAt *time.Time) (Project, error) {
	return s.repo.AdvanceStage(ctx, projectID, actorID, s.reviewDeadline(), expectedUpdatedAt)
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
		// Та же граница, что и при создании (см. StartProject): иначе
		// проверку на создании обходят переименованием.
		if v := strings.TrimSpace(*in.Title); utf8.RuneCountInString(v) < 3 {
			return Project{}, fmt.Errorf(
				"%w: название проекта — минимум 3 символа", ErrInvalidInput)
		}
	}
	if in.Budget != nil && *in.Budget < 0 {
		return Project{}, fmt.Errorf("%w: budget must be >= 0", ErrInvalidInput)
	}
	return s.repo.PatchProject(ctx, projectID, in)
}

// AssertManagerHasAccess — проверяет, что переданный пользователь является
// ответственным менеджером проекта. managerID=uuid.Nil интерпретируется
// как «от админа» — assigned_to-фильтр пропускается, только верифицируем
// что проект существует.
func (s *Service) AssertManagerHasAccess(ctx context.Context, projectID, managerID uuid.UUID) error {
	_, err := s.repo.GetByIDForManager(ctx, projectID, managerID)
	return err
}

// MoveProjectToStage — переставить проект на произвольную стадию (любую,
// не только следующую). Защита: если в текущей стадии есть незавершённый
// клиентский шаг — отказ. Для движения «вперёд» промежуточные team-шаги
// делаются done. Для движения «назад» — целевая и последующие стадии
// сбрасываются в pending, активируется первый шаг целевой стадии.
// Эмитит project.stage_moved для outbox (хук под будущие n8n-колбэки).
func (s *Service) MoveProjectToStage(ctx context.Context, projectID, targetStageID, actorID uuid.UUID, expectedUpdatedAt *time.Time) (Project, error) {
	return s.repo.MoveProjectToStage(ctx, projectID, targetStageID, actorID, s.reviewDeadline(), expectedUpdatedAt)
}

// MoveProjectToStep — точечный перенос проекта на конкретный шаг (для
// канбана по шагам). См. repo.MoveProjectToStep.
func (s *Service) MoveProjectToStep(ctx context.Context, projectID, targetStepID, actorID uuid.UUID, expectedUpdatedAt *time.Time) (Project, error) {
	return s.repo.MoveProjectToStep(ctx, projectID, targetStepID, actorID, s.reviewDeadline(), expectedUpdatedAt)
}

// ChangeFunnel — поменять воронку у проекта. Прогресс сбрасывается:
// project_stages / project_steps удаляются и инстанциируются заново
// из новой воронки. См. repo.ChangeFunnel.
func (s *Service) ChangeFunnel(ctx context.Context, projectID, newPipelineID, actorID uuid.UUID) (Project, error) {
	return s.repo.ChangeFunnel(ctx, projectID, newPipelineID, actorID)
}

// turnkeyIDs — только проекты «креаторы под ключ»: у остальных прогресс
// считается по шагам, и лишний запрос им не нужен.
func turnkeyIDs(projects []Project) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		if p.Kind == KindCreatorsTurnkey {
			out = append(out, p.ID)
		}
	}
	return out
}

// turnkeyProgress — доля закрытых выкладок одного проекта. Второй
// результат — «мера применима»: у проекта с воронкой её нет.
func (s *Service) turnkeyProgress(ctx context.Context, p Project) (float64, bool) {
	if p.Kind != KindCreatorsTurnkey {
		return 0, false
	}
	m, err := s.repo.PublicationProgress(ctx, []uuid.UUID{p.ID})
	if err != nil {
		return 0, false
	}
	pct, ok := m[p.ID]
	return pct, ok
}
