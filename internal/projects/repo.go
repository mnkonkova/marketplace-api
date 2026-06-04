package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound        = errors.New("project not found")
	ErrForbidden       = errors.New("project not visible to user")
	ErrStepNotFound    = errors.New("project step not found")
	ErrInvalidTransition = errors.New("invalid step transition")
	ErrPipelineEmpty   = errors.New("pipeline has no stages or steps")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// StartProject — атомарно создать проект + снэпшот стадий и шагов.
// eta_date считается от StartedAt + накопленные duration_days по sort_order.
// Возвращает id созданного проекта.
func (r *Repo) StartProject(ctx context.Context, in StartProjectInput) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Считать revisions_included из пайплайна (для снимка в проект).
	var revs int
	if err := tx.QueryRow(ctx,
		`SELECT revisions_included FROM pipelines WHERE id = $1 AND is_active = TRUE`,
		in.PipelineID).Scan(&revs); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("%w: pipeline missing or inactive", ErrNotFound)
		}
		return uuid.Nil, fmt.Errorf("load pipeline: %w", err)
	}

	startedAt := in.StartedAt
	if startedAt == nil {
		now := time.Now()
		startedAt = &now
	}

	// 2. Создать проект (status=active, started_at=startedAt).
	// NULLIF на client_name/contact — пустые строки храним как NULL,
	// чтобы CHECK constraint видел их как «не задано».
	var projectID uuid.UUID
	if err := tx.QueryRow(ctx, `
INSERT INTO projects
  (lead_id, lead_recipient_specialist_id, client_user_id, client_name, client_contact,
   specialist_user_id, assigned_to_user_id,
   pipeline_id, title, source, status, revisions_included, budget, notes, started_at)
VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7,$8,$9,$10,'active',$11,$12,$13,$14)
RETURNING id`,
		in.LeadID, in.LeadRecipientSpecialistID, in.ClientUserID, in.ClientName, in.ClientContact,
		in.SpecialistUserID, in.AssignedToUserID,
		in.PipelineID, in.Title, string(in.Source), revs, in.Budget, in.Notes, startedAt,
	).Scan(&projectID); err != nil {
		return uuid.Nil, fmt.Errorf("insert project: %w", err)
	}

	// 3-5. Снэпшот стадий и шагов + активация первой стадии — общий helper,
	// используется и при создании, и при ChangeFunnel (там тот же flow с нуля).
	if err := materializePipeline(ctx, tx, projectID, in.PipelineID, *startedAt); err != nil {
		return uuid.Nil, err
	}

	// 6. Событие created — для outbox/n8n (Ф8 повесит на этот event email).
	// actorID для no-account-клиента = uuid.Nil; в payload client_user_id
	// либо UUID, либо null (отдельные no-account-клиенты идентифицируются
	// по project.client_name/client_contact).
	var actor uuid.UUID
	payload := map[string]any{"project_id": projectID.String()}
	if in.ClientUserID != nil {
		actor = *in.ClientUserID
		payload["client_user_id"] = in.ClientUserID.String()
	} else {
		payload["client_name"] = in.ClientName
		payload["client_contact"] = in.ClientContact
	}
	if err := emit(ctx, tx, projectID, nil, actor, "project.created", payload); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return projectID, nil
}

// materializePipeline — снапшот стадий/шагов выбранной воронки в
// project_stages/project_steps + активация первой стадии и первого шага.
// startedAt используется как точка отсчёта для eta_date (накопительно по
// duration_days). Применяется при создании проекта (StartProject) и при
// смене воронки (ChangeFunnel) — обе операции стартуют шаги с нуля.
func materializePipeline(
	ctx context.Context,
	tx pgx.Tx,
	projectID, pipelineID uuid.UUID,
	startedAt time.Time,
) error {
	stageRows, err := tx.Query(ctx,
		`SELECT id, name, sort_order FROM pipeline_stages
		 WHERE pipeline_id = $1 ORDER BY sort_order, created_at`,
		pipelineID)
	if err != nil {
		return fmt.Errorf("list pipeline stages: %w", err)
	}
	type stageMapEntry struct {
		PipelineStageID uuid.UUID
		ProjectStageID  uuid.UUID
		Name            string
		SortOrder       int
	}
	stagesByPipelineID := map[uuid.UUID]uuid.UUID{}
	var stages []stageMapEntry
	for stageRows.Next() {
		var pid uuid.UUID
		var name string
		var sortOrder int
		if err := stageRows.Scan(&pid, &name, &sortOrder); err != nil {
			stageRows.Close()
			return fmt.Errorf("scan pipeline stage: %w", err)
		}
		stages = append(stages, stageMapEntry{PipelineStageID: pid, Name: name, SortOrder: sortOrder})
	}
	stageRows.Close()
	if err := stageRows.Err(); err != nil {
		return fmt.Errorf("iter pipeline stages: %w", err)
	}
	if len(stages) == 0 {
		return ErrPipelineEmpty
	}

	for i := range stages {
		var psID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO project_stages (project_id, name, sort_order)
			 VALUES ($1,$2,$3) RETURNING id`,
			projectID, stages[i].Name, stages[i].SortOrder).Scan(&psID); err != nil {
			return fmt.Errorf("insert project stage: %w", err)
		}
		stages[i].ProjectStageID = psID
		stagesByPipelineID[stages[i].PipelineStageID] = psID
	}

	stageIDs := make([]uuid.UUID, 0, len(stages))
	for _, st := range stages {
		stageIDs = append(stageIDs, st.PipelineStageID)
	}
	stepRows, err := tx.Query(ctx, `
SELECT s.id, s.stage_id, s.name, s.owner, s.duration_days,
       s.visible_to_client, s.visible_to_specialist, s.weight, s.sort_order, s.is_review
FROM pipeline_steps s
JOIN pipeline_stages st ON st.id = s.stage_id
WHERE s.stage_id = ANY($1)
ORDER BY st.sort_order, s.sort_order, s.created_at`, stageIDs)
	if err != nil {
		return fmt.Errorf("list pipeline steps: %w", err)
	}
	type rawStep struct {
		StageID             uuid.UUID
		Name                string
		Owner               string
		DurationDays        int
		VisibleToClient     bool
		VisibleToSpecialist bool
		Weight              int
		SortOrder           int
		IsReview            bool
	}
	var rawSteps []rawStep
	for stepRows.Next() {
		var s rawStep
		var ignored uuid.UUID
		if err := stepRows.Scan(&ignored, &s.StageID, &s.Name, &s.Owner, &s.DurationDays,
			&s.VisibleToClient, &s.VisibleToSpecialist, &s.Weight, &s.SortOrder, &s.IsReview); err != nil {
			stepRows.Close()
			return fmt.Errorf("scan pipeline step: %w", err)
		}
		rawSteps = append(rawSteps, s)
	}
	stepRows.Close()
	if err := stepRows.Err(); err != nil {
		return fmt.Errorf("iter pipeline steps: %w", err)
	}
	if len(rawSteps) == 0 {
		return ErrPipelineEmpty
	}

	cursor := startedAt
	for i, raw := range rawSteps {
		projectStageID, ok := stagesByPipelineID[raw.StageID]
		if !ok {
			return fmt.Errorf("stage mapping missing for %s", raw.StageID)
		}
		cursor = cursor.AddDate(0, 0, raw.DurationDays)
		eta := cursor

		// Первый шаг проекта стартует в in_progress (см. StartProject — то же
		// правило: «работа уже идёт» с момента старта).
		status := StepStatusPending
		var stepStartedAt *time.Time
		if i == 0 {
			status = StepStatusInProgress
			s := startedAt
			stepStartedAt = &s
		}

		if _, err := tx.Exec(ctx, `
INSERT INTO project_steps
  (project_id, stage_id, name, owner, status, duration_days,
   visible_to_client, visible_to_specialist, weight, sort_order, is_review,
   eta_date, started_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			projectID, projectStageID, raw.Name, raw.Owner, string(status), raw.DurationDays,
			raw.VisibleToClient, raw.VisibleToSpecialist, raw.Weight, raw.SortOrder, raw.IsReview,
			eta, stepStartedAt,
		); err != nil {
			return fmt.Errorf("insert project step: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE project_stages SET started_at = $2
		 WHERE project_id = $1 AND id = $3`,
		projectID, startedAt, stages[0].ProjectStageID); err != nil {
		return fmt.Errorf("activate first stage: %w", err)
	}
	return nil
}

// ChangeFunnel — поменять воронку у проекта. Старые project_stages /
// project_steps удаляются (CASCADE через FK), новая воронка инстанциируется
// с нуля. revisions_used обнуляется, started_at пересчитывается. Применяется
// админом из карточки проекта.
//
// Не делаем optimistic-lock: операция явная (модалка с подтверждением),
// race-condition тут маловероятен.
func (r *Repo) ChangeFunnel(
	ctx context.Context,
	projectID, newPipelineID, actorID uuid.UUID,
) (Project, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Project{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Берём проект под FOR UPDATE — блокируем от параллельных move/advance.
	var oldPipelineID uuid.UUID
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT pipeline_id, status FROM projects WHERE id = $1 FOR UPDATE`,
		projectID).Scan(&oldPipelineID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		return Project{}, fmt.Errorf("load project: %w", err)
	}
	if status != "active" {
		return Project{}, fmt.Errorf("%w: cannot change funnel of non-active project", ErrInvalidTransition)
	}

	// 2. Снимок revisions_included из новой воронки + валидация is_active.
	var revs int
	if err := tx.QueryRow(ctx,
		`SELECT revisions_included FROM pipelines WHERE id = $1 AND is_active = TRUE`,
		newPipelineID).Scan(&revs); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, fmt.Errorf("%w: new pipeline missing or inactive", ErrNotFound)
		}
		return Project{}, fmt.Errorf("load new pipeline: %w", err)
	}

	// 3. Снести старые стадии (CASCADE убьёт project_steps + project_events,
	// привязанные к step_id; project_events с step_id=NULL сохранятся).
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_stages WHERE project_id = $1`, projectID); err != nil {
		return Project{}, fmt.Errorf("delete old stages: %w", err)
	}

	// 4. Перепривязать проект к новой воронке + сбросить прогресс.
	now := time.Now()
	if _, err := tx.Exec(ctx, `
UPDATE projects SET
  pipeline_id        = $2,
  revisions_included = $3,
  revisions_used     = 0,
  status             = 'active',
  completed_at       = NULL,
  started_at         = $4,
  updated_at         = now()
WHERE id = $1`, projectID, newPipelineID, revs, now); err != nil {
		return Project{}, fmt.Errorf("update project: %w", err)
	}

	// 5. Инстанциируем новые project_stages/project_steps.
	if err := materializePipeline(ctx, tx, projectID, newPipelineID, now); err != nil {
		return Project{}, err
	}

	// 6. Событие в outbox/audit для воркера и истории проекта.
	payload := map[string]any{
		"project_id":      projectID.String(),
		"old_pipeline_id": oldPipelineID.String(),
		"new_pipeline_id": newPipelineID.String(),
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.funnel_changed", payload); err != nil {
		return Project{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetByID(ctx, projectID)
}

// GetByIDForClient — проект клиента; ErrForbidden если клиент не владелец.
func (r *Repo) GetByIDForClient(ctx context.Context, projectID, clientID uuid.UUID) (Project, error) {
	return r.getByID(ctx, projectID, &clientID, "client_user_id")
}

// GetByID — без проверки доступа (для admin/manager). Внутренний.
func (r *Repo) GetByID(ctx context.Context, projectID uuid.UUID) (Project, error) {
	return r.getByID(ctx, projectID, nil, "")
}

func (r *Repo) getByID(ctx context.Context, projectID uuid.UUID, userIDFilter *uuid.UUID, col string) (Project, error) {
	q := `
SELECT id, lead_id, lead_recipient_specialist_id, client_user_id,
       COALESCE(client_name,''), COALESCE(client_contact,''),
       specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects WHERE id = $1`
	args := []any{projectID}
	if userIDFilter != nil {
		q += " AND " + col + " = $2"
		args = append(args, *userIDFilter)
	}
	var p Project
	err := r.db.QueryRow(ctx, q, args...).Scan(
		&p.ID, &p.LeadID, &p.LeadRecipientSpecialistID, &p.ClientUserID,
		&p.ClientName, &p.ClientContact,
		&p.SpecialistUserID, &p.AssignedToUserID,
		&p.PipelineID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed, &p.Budget,
		&p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if userIDFilter != nil {
			// различить «нет такого» от «не твой» неважно для клиента —
			// отдаём 404 для обоих кейсов (anti-enumeration).
			return Project{}, ErrNotFound
		}
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// ListForClient — проекты клиента, отсортированные по дате создания.
// cancelled скрываем: у них 30-дневная retention до физического удаления
// (см. cleanup.go), в кабинете клиента им делать нечего — только засоряют
// «Мои проекты». Симметрично админскому ListAll по умолчанию.
func (r *Repo) ListForClient(ctx context.Context, clientID uuid.UUID) ([]Project, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, lead_id, lead_recipient_specialist_id, client_user_id,
       COALESCE(client_name,''), COALESCE(client_contact,''),
       specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects
WHERE client_user_id = $1 AND status <> 'cancelled'
ORDER BY created_at DESC`, clientID)
	if err != nil {
		return nil, fmt.Errorf("list client projects: %w", err)
	}
	defer rows.Close()
	out := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(
			&p.ID, &p.LeadID, &p.LeadRecipientSpecialistID, &p.ClientUserID,
			&p.ClientName, &p.ClientContact,
			&p.SpecialistUserID, &p.AssignedToUserID,
			&p.PipelineID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed, &p.Budget,
			&p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LoadStagesBatch — стадии для пачки проектов одним SELECT. Группирует по
// project_id, сохраняя порядок sort_order. Используется для list-эндпоинтов
// (клиентский /me/projects, менеджерский inbox/канбан, админский /admin/projects),
// чтобы избежать N+1 при enrich-е.
func (r *Repo) LoadStagesBatch(ctx context.Context, projectIDs []uuid.UUID) (map[uuid.UUID][]Stage, error) {
	out := make(map[uuid.UUID][]Stage, len(projectIDs))
	if len(projectIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT id, project_id, name, sort_order, started_at, completed_at
FROM project_stages
WHERE project_id = ANY($1)
ORDER BY project_id, sort_order`, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("load stages batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s Stage
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.Name, &s.SortOrder, &s.StartedAt, &s.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan stage: %w", err)
		}
		out[s.ProjectID] = append(out[s.ProjectID], s)
	}
	return out, rows.Err()
}

// LoadStepsBatch — шаги для пачки проектов одним SELECT. Парная LoadStagesBatch.
// onlyVisibleToClient=true фильтрует скрытые от клиента — для клиентского списка.
func (r *Repo) LoadStepsBatch(ctx context.Context, projectIDs []uuid.UUID, onlyVisibleToClient bool) (map[uuid.UUID][]Step, error) {
	out := make(map[uuid.UUID][]Step, len(projectIDs))
	if len(projectIDs) == 0 {
		return out, nil
	}
	q := `
SELECT ps.id, ps.project_id, ps.stage_id, ps.name, ps.owner, ps.status,
       ps.duration_days, ps.visible_to_client, ps.visible_to_specialist,
       ps.weight, ps.sort_order, ps.is_review, ps.eta_date, ps.review_deadline,
       ps.started_at, ps.completed_at, ps.created_at, ps.updated_at
FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
WHERE ps.project_id = ANY($1)`
	if onlyVisibleToClient {
		q += " AND ps.visible_to_client = TRUE"
	}
	q += " ORDER BY ps.project_id, st.sort_order, ps.sort_order"
	rows, err := r.db.Query(ctx, q, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("load steps batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s Step
		if err := rows.Scan(
			&s.ID, &s.ProjectID, &s.StageID, &s.Name, &s.Owner, &s.Status,
			&s.DurationDays, &s.VisibleToClient, &s.VisibleToSpecialist,
			&s.Weight, &s.SortOrder, &s.IsReview, &s.EtaDate, &s.ReviewDeadline,
			&s.StartedAt, &s.CompletedAt, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		out[s.ProjectID] = append(out[s.ProjectID], s)
	}
	return out, rows.Err()
}

// LoadStages — все стадии проекта в порядке sort_order.
func (r *Repo) LoadStages(ctx context.Context, projectID uuid.UUID) ([]Stage, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, project_id, name, sort_order, started_at, completed_at
		 FROM project_stages WHERE project_id = $1 ORDER BY sort_order`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("load stages: %w", err)
	}
	defer rows.Close()
	out := make([]Stage, 0)
	for rows.Next() {
		var s Stage
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.Name, &s.SortOrder, &s.StartedAt, &s.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan stage: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LoadSteps — все шаги проекта по sort_order стадии и шага.
// onlyVisibleToClient=true фильтрует скрытые от клиента.
func (r *Repo) LoadSteps(ctx context.Context, projectID uuid.UUID, onlyVisibleToClient bool) ([]Step, error) {
	q := `
SELECT ps.id, ps.project_id, ps.stage_id, ps.name, ps.owner, ps.status,
       ps.duration_days, ps.visible_to_client, ps.visible_to_specialist,
       ps.weight, ps.sort_order, ps.is_review, ps.eta_date, ps.review_deadline,
       ps.started_at, ps.completed_at, ps.created_at, ps.updated_at
FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
WHERE ps.project_id = $1`
	if onlyVisibleToClient {
		q += " AND ps.visible_to_client = TRUE"
	}
	q += " ORDER BY st.sort_order, ps.sort_order"
	rows, err := r.db.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("load steps: %w", err)
	}
	defer rows.Close()
	out := make([]Step, 0)
	for rows.Next() {
		var s Step
		if err := rows.Scan(
			&s.ID, &s.ProjectID, &s.StageID, &s.Name, &s.Owner, &s.Status,
			&s.DurationDays, &s.VisibleToClient, &s.VisibleToSpecialist,
			&s.Weight, &s.SortOrder, &s.IsReview, &s.EtaDate, &s.ReviewDeadline,
			&s.StartedAt, &s.CompletedAt, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetStep — один шаг по id внутри проекта (для проверки принадлежности).
func (r *Repo) GetStep(ctx context.Context, projectID, stepID uuid.UUID) (Step, error) {
	var s Step
	err := r.db.QueryRow(ctx, `
SELECT id, project_id, stage_id, name, owner, status, duration_days,
       visible_to_client, visible_to_specialist, weight, sort_order, is_review,
       eta_date, review_deadline, started_at, completed_at, created_at, updated_at
FROM project_steps WHERE id = $1 AND project_id = $2`, stepID, projectID).Scan(
		&s.ID, &s.ProjectID, &s.StageID, &s.Name, &s.Owner, &s.Status, &s.DurationDays,
		&s.VisibleToClient, &s.VisibleToSpecialist, &s.Weight, &s.SortOrder, &s.IsReview,
		&s.EtaDate, &s.ReviewDeadline, &s.StartedAt, &s.CompletedAt, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrStepNotFound
	}
	if err != nil {
		return Step{}, fmt.Errorf("get step: %w", err)
	}
	return s, nil
}

// TransitionStep — стейт-машинный переход. WithIncrementRevisions=true
// для rejected: подбамп projects.revisions_used. Если RevisionsUsed после
// инкремента превысит RevisionsIncluded — проект переводится в dispute и
// возвращается флаг disputed=true (caller может эмитнуть отдельное событие).
type TransitionInput struct {
	ProjectID          uuid.UUID
	StepID             uuid.UUID
	From               StepStatus // ожидаемое from (для optimistic-проверки)
	To                 StepStatus
	ActorUserID        uuid.UUID
	ActorType          string // human|system
	Comment            string
	IncrementRevisions bool
	SetReviewDeadline  *time.Time // для is_review при waiting_client
}

type TransitionResult struct {
	Project   Project
	Step      Step
	Disputed  bool // если в transition подбампали revisions_used и упёрлись в лимит
}

func (r *Repo) TransitionStep(ctx context.Context, in TransitionInput) (TransitionResult, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now()

	// 1. Обновляем шаг с проверкой текущего статуса (optimistic state).
	var completedAt *time.Time
	if in.To == StepStatusDone || in.To == StepStatusSkipped {
		completedAt = &now
	}
	var startedAt *time.Time
	if in.From == StepStatusPending && in.To == StepStatusInProgress {
		startedAt = &now
	}

	tag, err := tx.Exec(ctx, `
UPDATE project_steps SET
  status          = $3,
  started_at      = COALESCE(started_at, $4),
  completed_at    = CASE WHEN $5::timestamptz IS NOT NULL THEN $5 ELSE completed_at END,
  review_deadline = COALESCE($6, review_deadline),
  updated_at      = now()
WHERE id = $1 AND project_id = $2 AND status = $7`,
		in.StepID, in.ProjectID, string(in.To),
		startedAt, completedAt, in.SetReviewDeadline, string(in.From))
	if err != nil {
		return TransitionResult{}, fmt.Errorf("update step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// либо шага нет, либо статус уже не From — для caller обе ветки = 409
		return TransitionResult{}, ErrInvalidTransition
	}

	// 2. revisions_used + dispute (только при IncrementRevisions).
	disputed := false
	if in.IncrementRevisions {
		var newUsed, included int
		if err := tx.QueryRow(ctx,
			`UPDATE projects SET revisions_used = revisions_used + 1, updated_at = now()
			 WHERE id = $1 RETURNING revisions_used, revisions_included`,
			in.ProjectID).Scan(&newUsed, &included); err != nil {
			return TransitionResult{}, fmt.Errorf("bump revisions: %w", err)
		}
		if newUsed > included {
			if _, err := tx.Exec(ctx,
				`UPDATE projects SET status = 'dispute', updated_at = now() WHERE id = $1`,
				in.ProjectID); err != nil {
				return TransitionResult{}, fmt.Errorf("set dispute: %w", err)
			}
			disputed = true
		}
	}

	// 3. Событие в project_step_events (лента активности).
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, from_status, to_status, comment)
VALUES ($1,$2,$3,$4,'step_transition',$5,$6,$7)`,
		in.ProjectID, in.StepID, in.ActorUserID, in.ActorType,
		string(in.From), string(in.To), in.Comment); err != nil {
		return TransitionResult{}, fmt.Errorf("insert event: %w", err)
	}

	// 4. Outbox-событие для нотификаций.
	if err := emit(ctx, tx, in.ProjectID, &in.StepID, in.ActorUserID, "project.step_transitioned",
		map[string]any{
			"project_id":  in.ProjectID.String(),
			"step_id":     in.StepID.String(),
			"from":        string(in.From),
			"to":          string(in.To),
			"actor_type":  in.ActorType,
		},
	); err != nil {
		return TransitionResult{}, err
	}
	if disputed {
		if err := emit(ctx, tx, in.ProjectID, nil, in.ActorUserID, "project.disputed",
			map[string]string{"project_id": in.ProjectID.String()},
		); err != nil {
			return TransitionResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return TransitionResult{}, fmt.Errorf("commit: %w", err)
	}

	// 5. Подгружаем обновлённый проект/шаг для возврата.
	p, err := r.GetByID(ctx, in.ProjectID)
	if err != nil {
		return TransitionResult{}, err
	}
	s, err := r.GetStep(ctx, in.ProjectID, in.StepID)
	if err != nil {
		return TransitionResult{}, err
	}
	return TransitionResult{Project: p, Step: s, Disputed: disputed}, nil
}

// FindPrevTeamStepInStage — последний team-шаг в той же стадии с меньшим
// sort_order и статусом done. Используется в request_revision для возврата.
// Если в текущей стадии нет — поднимаемся по стадиям.
func (r *Repo) FindPrevTeamStep(ctx context.Context, projectID, currentStepID uuid.UUID) (Step, error) {
	var s Step
	err := r.db.QueryRow(ctx, `
WITH cur AS (
    SELECT ps.stage_id, ps.sort_order, st.sort_order AS stage_order
    FROM project_steps ps JOIN project_stages st ON st.id = ps.stage_id
    WHERE ps.id = $1 AND ps.project_id = $2
)
SELECT ps.id, ps.project_id, ps.stage_id, ps.name, ps.owner, ps.status, ps.duration_days,
       ps.visible_to_client, ps.visible_to_specialist, ps.weight, ps.sort_order, ps.is_review,
       ps.eta_date, ps.review_deadline, ps.started_at, ps.completed_at, ps.created_at, ps.updated_at
FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
JOIN cur ON TRUE
WHERE ps.project_id = $2
  AND ps.owner = 'team'
  AND ps.status = 'done'
  AND (st.sort_order, ps.sort_order) < (cur.stage_order, cur.sort_order)
ORDER BY st.sort_order DESC, ps.sort_order DESC
LIMIT 1`, currentStepID, projectID).Scan(
		&s.ID, &s.ProjectID, &s.StageID, &s.Name, &s.Owner, &s.Status, &s.DurationDays,
		&s.VisibleToClient, &s.VisibleToSpecialist, &s.Weight, &s.SortOrder, &s.IsReview,
		&s.EtaDate, &s.ReviewDeadline, &s.StartedAt, &s.CompletedAt, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrStepNotFound
	}
	if err != nil {
		return Step{}, fmt.Errorf("find prev team step: %w", err)
	}
	return s, nil
}

// emit — обёртка над outbox.Emit, чтобы не тащить импорт outbox в каждый repo-вызов.
// step_id опциональный (события стадии/проекта без шага).
func emit(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, stepID *uuid.UUID, actorID uuid.UUID, eventType string, payload any) error {
	// Используем outbox-таблицу напрямую вместо outbox.Emit, чтобы избежать
	// циклического импорта. Структура та же.
	const q = `INSERT INTO outbox (aggregate, aggregate_id, event_type, payload)
	           VALUES ('project', $1, $2, $3)`
	data, err := jsonMarshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx, q, projectID.String(), eventType, data); err != nil {
		return fmt.Errorf("emit outbox event: %w", err)
	}
	_ = stepID
	_ = actorID
	return nil
}
