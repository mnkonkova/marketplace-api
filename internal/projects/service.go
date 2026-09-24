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
	// ErrInvalidClientUser — попытка указать в client_user_id юзера,
	// который является менеджером/админом (data-sec D5). Раньше менеджер мог
	// прицепить проект к UUID коллеги/админа — у жертвы появлялся фейковый
	// проект в кабинете + раскрывались контакты при GetFull.
	ErrInvalidClientUser = errors.New("client_user_id refers to non-client (manager/admin)")
)

type Service struct {
	repo                    *Repo
	reviewDeadlineDuration  time.Duration
	defaultPipelineProvider DefaultPipelineProvider
	checklistAttacher       ChecklistAttacher
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
	// Нижняя граница — три символа, и считаем её ПОСЛЕ trim. Без неё в
	// списке заводятся «12345675432» и «  ы  »: по такому названию проект
	// не найти ни поиском, ни глазами, а переименовать его потом некому.
	// Двух символов хватает разве что на инициалы — для проекта это не
	// название.
	if n := utf8.RuneCountInString(in.Title); n < 3 {
		return uuid.Nil, fmt.Errorf(
			"%w: название проекта — минимум 3 символа", ErrInvalidInput)
	} else if n > 200 {
		return uuid.Nil, fmt.Errorf("%w: title is too long", ErrInvalidInput)
	}
	// Клиент задаётся либо через client_user_id (зарегистрированный),
	// либо через client_name+client_contact (заводит менеджер/админ для
	// клиента без аккаунта). Без того и другого создавать проект нельзя.
	hasContact := strings.TrimSpace(in.ClientName) != "" && strings.TrimSpace(in.ClientContact) != ""
	if in.ClientUserID == nil && !hasContact {
		return uuid.Nil, fmt.Errorf("%w: укажите либо client_user_id, либо client_name+client_contact", ErrInvalidInput)
	}
	// Нормализуем contact-поля: пустые строки сбрасываем, чтобы DB
	// видела NULL (под CHECK constraint).
	in.ClientName = strings.TrimSpace(in.ClientName)
	in.ClientContact = strings.TrimSpace(in.ClientContact)
	// Воронка обязательна только продакшну: у креаторов вместо неё
	// выкладки, у общего проекта — один срок.
	if in.Kind == "" {
		in.Kind = KindProductionTurnkey
	}
	// Воронку при создании больше не выбирают: в форме выбирают вид
	// проекта. Продакшну она всё ещё нужна как каркас шагов — берём
	// воронку по умолчанию, а править её будут уже внутри проекта.
	if in.Kind == KindProductionTurnkey && in.PipelineID == uuid.Nil {
		def, err := s.repo.DefaultPipelineID(ctx)
		if err != nil {
			return uuid.Nil, fmt.Errorf(
				"%w: воронки по умолчанию нет — назначьте её в разделе «Воронки»", ErrInvalidInput)
		}
		in.PipelineID = def
	}
	if in.Source == "" {
		in.Source = SourceManual
	}
	// data-sec D5: если client_user_id передан явно, проверяем что таргет —
	// обычный юзер (не менеджер и не админ). Без этой проверки менеджер
	// мог прицепить проект к UUID коллеги-менеджера или админа: жертва
	// видела бы фейковый проект в /me/projects и контакты менеджера.
	if in.ClientUserID != nil {
		if err := s.repo.AssertUserCanBeClient(ctx, *in.ClientUserID); err != nil {
			return uuid.Nil, err
		}
	}
	id, err := s.repo.StartProject(ctx, in)
	if err != nil {
		return uuid.Nil, err
	}
	// Чек-лист подключается сам. Менеджер заводит проект и уходит
	// собирать команду; вспоминают о чек-листе в момент первой сдачи —
	// то есть когда креатор уже снял ролик по своим представлениям.
	// Кто актор: тот, на кого проект назначен, иначе — никто.
	var actor uuid.UUID
	if in.AssignedToUserID != nil {
		actor = *in.AssignedToUserID
	}
	s.attachChecklist(ctx, id, actor, in.Kind)
	return id, nil
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
	primaryCats := map[uuid.UUID]string{}
	// Имена менеджеров — одним запросом на весь список, как и всё
	// остальное здесь: у заказчика проектов бывает десяток.
	managers := map[uuid.UUID]PartyContact{}
	mgrSet := map[uuid.UUID]bool{}
	for _, p := range projects {
		if p.AssignedToUserID != nil {
			mgrSet[*p.AssignedToUserID] = true
		}
	}
	if len(mgrSet) > 0 {
		mgrIDs := make([]uuid.UUID, 0, len(mgrSet))
		for id := range mgrSet {
			mgrIDs = append(mgrIDs, id)
		}
		// best-effort, как и имена исполнителей: без имени карточка
		// обходится, без списка проектов — нет.
		if m, err := s.repo.LoadPartyContacts(ctx, mgrIDs); err == nil {
			managers = m
		}
	}
	if len(specSet) > 0 {
		specIDs := make([]uuid.UUID, 0, len(specSet))
		for id := range specSet {
			specIDs = append(specIDs, id)
		}
		// best-effort: имена и primary category — для отображения. Ошибки
		// не валим запрос, карточка просто без опознавательного знака.
		if n, err := s.repo.LoadClientDisplayNames(ctx, specIDs); err == nil {
			names = n
		}
		if c, err := s.repo.LoadSpecialistPrimaryCategoryTitles(ctx, specIDs); err == nil {
			primaryCats = c
		}
	}
	// Прогресс проектов с креаторами — по выкладкам, одним запросом на
	// весь список: шагов у них нет, и прогресс по шагам всегда ноль.
	pubProgress, err := s.repo.PublicationProgress(ctx, turnkeyIDs(projects))
	if err != nil {
		pubProgress = map[uuid.UUID]float64{}
	}
	views := make([]ProjectClientView, 0, len(projects))
	for _, p := range projects {
		view := buildClientView(p, stagesByProject[p.ID], stepsByProject[p.ID])
		if pct, ok := pubProgress[p.ID]; ok {
			view.Progress = pct
		}
		if p.SpecialistUserID != nil {
			view.SpecialistDisplayName = names[*p.SpecialistUserID]
			view.SpecialistPrimaryCategory = primaryCats[*p.SpecialistUserID]
		}
		if p.AssignedToUserID != nil {
			view.ManagerDisplayName = managers[*p.AssignedToUserID].DisplayName
		}
		views = append(views, view)
	}
	return views, nil
}

// GetClientProject — полный проект клиента с воронкой.
func (s *Service) GetClientProject(ctx context.Context, projectID, clientID uuid.UUID) (ProjectClientView, error) {
	// Глазами заказчика — но не только заказчику: менеджеру проекта и
	// админу тоже, ради кнопки «посмотреть, как это видит клиент».
	p, err := s.repo.GetByIDAsClientView(ctx, projectID, clientID)
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
	// Заказчик проекта с креаторами меряет его выкладками, а не шагами:
	// шагов у такого проекта нет, и прогресс по ним всегда ноль.
	if pct, ok := s.turnkeyProgress(ctx, p); ok {
		view.Progress = pct
	}
	if p.SpecialistUserID != nil {
		// best-effort: имя specialist'а из specialist_profiles
		if names, err := s.repo.LoadClientDisplayNames(ctx, []uuid.UUID{*p.SpecialistUserID}); err == nil {
			view.SpecialistDisplayName = names[*p.SpecialistUserID]
		}
	}
	if p.AssignedToUserID != nil {
		// Тоже best-effort: проект без имени менеджера открывается,
		// проект, не открывшийся из-за имени, — нет.
		if m, err := s.repo.LoadPartyContacts(ctx, []uuid.UUID{*p.AssignedToUserID}); err == nil {
			view.ManagerDisplayName = m[*p.AssignedToUserID].DisplayName
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
