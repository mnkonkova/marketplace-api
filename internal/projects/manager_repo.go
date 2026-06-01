package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrAlreadyClaimed — POST /claim на проекте, у которого уже есть
	// assigned_to. Возвращаем 409 — другой менеджер успел раньше.
	ErrAlreadyClaimed = errors.New("project is already claimed")
	// ErrStageBlocked — advance_stage упёрся в незавершённый client-шаг.
	// Соответствует 409 в брифе §4.3.
	ErrStageBlocked = errors.New("stage cannot advance: pending client step")
	// ErrLastStage — попытка advance с последней стадии. На фронте отдадим
	// 409 и тост «проект уже на последней стадии».
	ErrLastStage = errors.New("no next stage to advance to")
	// ErrConflict — patch project прислал устаревший updated_at.
	ErrConflict = errors.New("project updated_at mismatch")
)

// ListInbox — проекты без ответственного (assigned_to_user_id IS NULL),
// для inbox-страницы менеджера. Сортировка — старые сверху, чтобы никого
// не «забыли».
func (r *Repo) ListInbox(ctx context.Context) ([]Project, error) {
	return r.listAssignedFilter(ctx, "WHERE assigned_to_user_id IS NULL ORDER BY created_at ASC")
}

// ListAssignedTo — проекты конкретного менеджера для канбана. Берём
// только не-терминальные (draft/active/on_hold/dispute) — done/cancelled
// в канбане не нужны.
func (r *Repo) ListAssignedTo(ctx context.Context, managerID uuid.UUID) ([]Project, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, lead_id, lead_recipient_id, client_user_id, specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects
WHERE assigned_to_user_id = $1
  AND status IN ('draft','active','on_hold','dispute')
ORDER BY updated_at DESC`, managerID)
	if err != nil {
		return nil, fmt.Errorf("list assigned projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

// ListAll — все проекты (для админского обзора + канбана). Можно
// расширить фильтрами через params, пока — простой список.
func (r *Repo) ListAll(ctx context.Context, statusFilter string) ([]Project, error) {
	q := `
SELECT id, lead_id, lead_recipient_id, client_user_id, specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects`
	args := []any{}
	if statusFilter != "" {
		q += " WHERE status = $1"
		args = append(args, statusFilter)
	}
	q += " ORDER BY updated_at DESC"
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list all projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

func (r *Repo) listAssignedFilter(ctx context.Context, suffix string) ([]Project, error) {
	q := `
SELECT id, lead_id, lead_recipient_id, client_user_id, specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects ` + suffix
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

func scanProjects(rows pgx.Rows) ([]Project, error) {
	out := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(
			&p.ID, &p.LeadID, &p.LeadRecipientID, &p.ClientUserID, &p.SpecialistUserID, &p.AssignedToUserID,
			&p.PipelineID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed, &p.Budget,
			&p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Claim — атомарно присвоить assigned_to_user_id, если ещё NULL. Иначе
// ErrAlreadyClaimed. Idempotent для самого менеджера (если уже его — ОК).
func (r *Repo) Claim(ctx context.Context, projectID, managerID uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
UPDATE projects SET assigned_to_user_id = $2, updated_at = now()
WHERE id = $1 AND (assigned_to_user_id IS NULL OR assigned_to_user_id = $2)`,
		projectID, managerID)
	if err != nil {
		return fmt.Errorf("claim project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// либо нет проекта, либо уже взят другим. Различить через probe.
		var existsTaken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1 AND assigned_to_user_id IS NOT NULL AND assigned_to_user_id <> $2)`,
			projectID, managerID).Scan(&existsTaken); err != nil {
			return fmt.Errorf("probe project: %w", err)
		}
		if existsTaken {
			return ErrAlreadyClaimed
		}
		return ErrNotFound
	}

	// Событие assigned для ленты активности.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'assigned', $3)`,
		projectID, managerID,
		fmt.Sprintf(`{"manager_user_id":"%s"}`, managerID.String())); err != nil {
		return fmt.Errorf("insert assigned event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, managerID, "project.assigned",
		map[string]string{"project_id": projectID.String(), "manager_user_id": managerID.String()},
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PatchProject — title/budget/notes c optimistic-lock. Bool+value тот же
// паттерн что в profiles, чтобы можно было занулять.
func (r *Repo) PatchProject(ctx context.Context, projectID uuid.UUID, in ManagerPatchInput) (Project, error) {
	const q = `
UPDATE projects SET
  title      = COALESCE($2, title),
  budget     = CASE WHEN $3::boolean THEN $4 ELSE budget END,
  notes      = COALESCE($5, notes),
  updated_at = now()
WHERE id = $1 AND ($6::timestamptz IS NULL OR updated_at = $6)
RETURNING id, lead_id, lead_recipient_id, client_user_id, specialist_user_id, assigned_to_user_id,
          pipeline_id, title, source, status, revisions_included, revisions_used, budget,
          COALESCE(notes,''), started_at, completed_at, created_at, updated_at`
	var trimmedTitle, trimmedNotes *string
	if in.Title != nil {
		v := strings.TrimSpace(*in.Title)
		trimmedTitle = &v
	}
	if in.Notes != nil {
		v := strings.TrimSpace(*in.Notes)
		trimmedNotes = &v
	}
	var p Project
	err := r.db.QueryRow(ctx, q,
		projectID,
		trimmedTitle,
		in.Budget != nil, in.Budget,
		trimmedNotes,
		in.UpdatedAt,
	).Scan(
		&p.ID, &p.LeadID, &p.LeadRecipientID, &p.ClientUserID, &p.SpecialistUserID, &p.AssignedToUserID,
		&p.PipelineID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed, &p.Budget,
		&p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// либо нет, либо updated_at не совпал
		if in.UpdatedAt != nil {
			// probe — есть ли вообще такой
			var exists bool
			if perr := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)`,
				projectID).Scan(&exists); perr == nil && exists {
				return Project{}, ErrConflict
			}
		}
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("patch project: %w", err)
	}
	return p, nil
}

// stageInfo — текущая стадия проекта (минимум для канбана и advance_stage).
type stageInfo struct {
	ID         uuid.UUID
	Name       string
	SortOrder  int
	StartedAt  *time.Time
	Completed  bool
}

// CurrentAndNextStage — текущая активная стадия (первая с шагами не в
// (done|skipped)) и следующая. Если current=nil — все шаги завершены
// (проект готов к закрытию). Используется в канбане и в advance_stage.
func (r *Repo) CurrentAndNextStage(ctx context.Context, projectID uuid.UUID) (current, next *stageInfo, err error) {
	rows, err := r.db.Query(ctx, `
SELECT st.id, st.name, st.sort_order, st.started_at,
       BOOL_AND(ps.status IN ('done','skipped')) FILTER (WHERE ps.id IS NOT NULL) AS all_done
FROM project_stages st
LEFT JOIN project_steps ps ON ps.stage_id = st.id
WHERE st.project_id = $1
GROUP BY st.id, st.name, st.sort_order, st.started_at
ORDER BY st.sort_order`, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("load stages with status: %w", err)
	}
	defer rows.Close()
	var stages []stageInfo
	for rows.Next() {
		var s stageInfo
		var allDone *bool
		if err := rows.Scan(&s.ID, &s.Name, &s.SortOrder, &s.StartedAt, &allDone); err != nil {
			return nil, nil, fmt.Errorf("scan stage: %w", err)
		}
		// Стадия без шагов — считаем not_done (защита от пустых).
		if allDone != nil && *allDone {
			s.Completed = true
		}
		stages = append(stages, s)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range stages {
		if !stages[i].Completed {
			c := stages[i]
			current = &c
			if i+1 < len(stages) {
				n := stages[i+1]
				next = &n
			}
			return
		}
	}
	// все стадии завершены → current=nil
	return nil, nil, nil
}

// AdvanceStage — переход на следующую стадию. Бизнес-логика (бриф §4.3):
//   1. Если в текущей стадии есть незавершённый client-шаг → ErrStageBlocked.
//   2. Все pending/in_progress team-шаги текущей стадии → done.
//   3. Текущая стадия completed_at = now.
//   4. Первый шаг следующей стадии активируется:
//      owner=team/system → in_progress; owner=client → waiting_client.
//   5. Следующая стадия started_at = now.
//   6. Event stage_advance + outbox.
// Если следующей стадии нет — ErrLastStage.
func (r *Repo) AdvanceStage(ctx context.Context, projectID, actorID uuid.UUID) (Project, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Project{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Используем CurrentAndNext через ту же таблицу — но в транзакции,
	// чтобы не было гонки. Перечитываем стадии под FOR UPDATE.
	rows, err := tx.Query(ctx, `
SELECT st.id, st.sort_order
FROM project_stages st
WHERE st.project_id = $1
ORDER BY st.sort_order
FOR UPDATE`, projectID)
	if err != nil {
		return Project{}, fmt.Errorf("lock stages: %w", err)
	}
	type stRow struct {
		ID    uuid.UUID
		Order int
	}
	var stages []stRow
	for rows.Next() {
		var s stRow
		if err := rows.Scan(&s.ID, &s.Order); err != nil {
			rows.Close()
			return Project{}, fmt.Errorf("scan stage row: %w", err)
		}
		stages = append(stages, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Project{}, err
	}
	if len(stages) == 0 {
		return Project{}, ErrNotFound
	}

	// 1. Найти первую стадию, где НЕ все шаги done/skipped.
	var currentIdx = -1
	for i, st := range stages {
		var allDone bool
		if err := tx.QueryRow(ctx, `
SELECT BOOL_AND(status IN ('done','skipped'))
FROM project_steps WHERE stage_id = $1`, st.ID).Scan(&allDone); err != nil {
			return Project{}, fmt.Errorf("check stage done: %w", err)
		}
		if !allDone {
			currentIdx = i
			break
		}
	}
	if currentIdx == -1 {
		return Project{}, ErrLastStage
	}
	current := stages[currentIdx]
	if currentIdx+1 >= len(stages) {
		return Project{}, ErrLastStage
	}
	next := stages[currentIdx+1]

	// 2. Проверка: в текущей стадии нет client-шага в pending/in_progress/waiting_client.
	var hasClientPending bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM project_steps
              WHERE stage_id = $1 AND owner = 'client'
                AND status IN ('pending','in_progress','waiting_client','rejected'))`,
		current.ID).Scan(&hasClientPending); err != nil {
		return Project{}, fmt.Errorf("check client pending: %w", err)
	}
	if hasClientPending {
		return Project{}, ErrStageBlocked
	}

	// 3. Все team/system pending|in_progress|rejected текущей стадии → done.
	if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status = 'done', completed_at = COALESCE(completed_at, now()), updated_at = now()
WHERE stage_id = $1 AND owner IN ('team','system')
  AND status IN ('pending','in_progress','rejected')`,
		current.ID); err != nil {
		return Project{}, fmt.Errorf("complete team steps: %w", err)
	}

	// 4. completed_at у текущей стадии.
	if _, err := tx.Exec(ctx,
		`UPDATE project_stages SET completed_at = COALESCE(completed_at, now())
		 WHERE id = $1`, current.ID); err != nil {
		return Project{}, fmt.Errorf("complete stage: %w", err)
	}

	// 5. Активация первого шага следующей стадии (по sort_order).
	var firstStepID uuid.UUID
	var firstStepOwner string
	err = tx.QueryRow(ctx, `
SELECT id, owner FROM project_steps
WHERE stage_id = $1 ORDER BY sort_order LIMIT 1`, next.ID).Scan(&firstStepID, &firstStepOwner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Project{}, fmt.Errorf("first step of next stage: %w", err)
	}
	if err == nil {
		newStatus := StepStatusInProgress
		if firstStepOwner == OwnerClient {
			newStatus = StepStatusWaitingClient
		}
		if _, err := tx.Exec(ctx, `
UPDATE project_steps SET status = $2, started_at = COALESCE(started_at, now()), updated_at = now()
WHERE id = $1 AND status = 'pending'`, firstStepID, string(newStatus)); err != nil {
			return Project{}, fmt.Errorf("activate next step: %w", err)
		}
	}

	// 6. started_at у новой стадии.
	if _, err := tx.Exec(ctx,
		`UPDATE project_stages SET started_at = COALESCE(started_at, now())
		 WHERE id = $1`, next.ID); err != nil {
		return Project{}, fmt.Errorf("start next stage: %w", err)
	}

	// 7. Бамп projects.updated_at + событие в ленте + outbox.
	if _, err := tx.Exec(ctx,
		`UPDATE projects SET updated_at = now() WHERE id = $1`, projectID); err != nil {
		return Project{}, fmt.Errorf("bump project: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'stage_advance', $3)`,
		projectID, actorID,
		fmt.Sprintf(`{"from_stage_id":"%s","to_stage_id":"%s"}`, current.ID, next.ID)); err != nil {
		return Project{}, fmt.Errorf("insert stage_advance event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.stage_advanced",
		map[string]string{
			"project_id":    projectID.String(),
			"from_stage_id": current.ID.String(),
			"to_stage_id":   next.ID.String(),
		},
	); err != nil {
		return Project{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetByID(ctx, projectID)
}

// GetByIDForManager — проект, видимый менеджеру (assigned_to_user_id = me).
// Без жёсткой проверки: если managerID=uuid.Nil — отдаст любой (для админа).
func (r *Repo) GetByIDForManager(ctx context.Context, projectID, managerID uuid.UUID) (Project, error) {
	if managerID == uuid.Nil {
		return r.GetByID(ctx, projectID)
	}
	return r.getByID(ctx, projectID, &managerID, "assigned_to_user_id")
}

// LoadClientDisplayNames — батч display_name для списка projects (для карточек
// канбана). Возвращает map client_user_id → display_name. Где нет имени —
// пустая строка.
func (r *Repo) LoadClientDisplayNames(ctx context.Context, clientIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(clientIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT user_id, display_name FROM specialist_profiles WHERE user_id = ANY($1)`,
		clientIDs)
	if err != nil {
		return nil, fmt.Errorf("load client names: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]string, len(clientIDs))
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}
