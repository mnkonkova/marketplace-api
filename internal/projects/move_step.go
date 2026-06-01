package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MoveProjectToStep — перевести проект на конкретный шаг (любой в воронке).
// Аналог MoveProjectToStage но точнее: целевая позиция = (stage_order,
// step_order), а не вся стадия.
//
// Алгоритм:
//   1. FOR UPDATE по project_stages + project_steps глобально упорядоченных
//      по (stage.sort_order, step.sort_order).
//   2. Резолвим target_step_id — может быть project_steps.id (snapshot) или
//      pipeline_steps.id (UI на канбане шлёт шаблонный id). Во втором случае
//      ищем project_step по совпадению (stage_order, step_order) у
//      соответствующего pipeline_step.
//   3. Находим текущую позицию — первый шаг не в (done|skipped).
//   4. target == current → no-op.
//   5. target > current (вперёд): промежуточные team/system → done,
//      промежуточные client → skipped (менеджер/админ имеют право обойти
//      клиентский шаг; клиентский кабинет этот endpoint не вызывает).
//      Стадии, у которых после этого все шаги в done|skipped, помечаются
//      completed_at.
//   6. target < current (назад): шаги target..current сбрасываются в pending,
//      review_deadline NULL'ится. Завершённости стадий обнуляются.
//   7. Активируем целевой шаг: owner=client → waiting_client (+ review_deadline
//      для is_review); team/system → in_progress.
//   8. Бамп projects.updated_at с optimistic-lock проверкой.
//   9. Events stage_moved (если стадия сменилась) + step_transition + outbox.
func (r *Repo) MoveProjectToStep(ctx context.Context, projectID, targetStepID, actorID uuid.UUID, reviewDeadline time.Duration, expectedUpdatedAt *time.Time) (Project, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Project{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Загружаем все шаги с глобальным порядком и блокируем строки.
	type stepRow struct {
		ID       uuid.UUID
		StageID  uuid.UUID
		Owner    string
		Status   StepStatus
		IsReview bool
		StageOrd int
		StepOrd  int
	}
	rows, err := tx.Query(ctx, `
SELECT ps.id, ps.stage_id, ps.owner, ps.status, ps.is_review,
       st.sort_order AS stage_ord, ps.sort_order AS step_ord
FROM project_steps ps
JOIN project_stages st ON st.id = ps.stage_id
WHERE ps.project_id = $1
ORDER BY st.sort_order, ps.sort_order
FOR UPDATE OF ps`, projectID)
	if err != nil {
		return Project{}, fmt.Errorf("lock steps: %w", err)
	}
	var steps []stepRow
	for rows.Next() {
		var s stepRow
		if err := rows.Scan(&s.ID, &s.StageID, &s.Owner, &s.Status, &s.IsReview, &s.StageOrd, &s.StepOrd); err != nil {
			rows.Close()
			return Project{}, fmt.Errorf("scan step: %w", err)
		}
		steps = append(steps, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Project{}, err
	}
	if len(steps) == 0 {
		return Project{}, ErrNotFound
	}

	// 2. Резолвим target.
	targetIdx := -1
	for i, s := range steps {
		if s.ID == targetStepID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		// Возможно UI передал pipeline_steps.id. Резолвим через order.
		var pipelineStageOrd, pipelineStepOrd int
		err := tx.QueryRow(ctx, `
SELECT pst.sort_order, ps.sort_order
FROM pipeline_steps ps JOIN pipeline_stages pst ON pst.id = ps.stage_id
WHERE ps.id = $1`, targetStepID).Scan(&pipelineStageOrd, &pipelineStepOrd)
		if err == nil {
			for i, s := range steps {
				if s.StageOrd == pipelineStageOrd && s.StepOrd == pipelineStepOrd {
					targetIdx = i
					break
				}
			}
		}
	}
	if targetIdx == -1 {
		return Project{}, ErrStepNotFound
	}

	// 3. Current = первый не в done|skipped.
	curIdx := -1
	for i, s := range steps {
		if s.Status != StepStatusDone && s.Status != StepStatusSkipped {
			curIdx = i
			break
		}
	}
	if curIdx == -1 {
		curIdx = len(steps) - 1
	}

	now := time.Now()

	// 4. no-op
	if targetIdx == curIdx {
		if err := r.bumpProject(ctx, tx, projectID, expectedUpdatedAt); err != nil {
			return Project{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Project{}, fmt.Errorf("commit: %w", err)
		}
		return r.GetByID(ctx, projectID)
	}

	fromStageID := steps[curIdx].StageID
	toStageID := steps[targetIdx].StageID

	if targetIdx > curIdx {
		// 5. Промежуточные шаги закрываем: team/system → done, client → skipped.
		// Endpoint вызывается только менеджером/админом; они вправе обойти
		// клиентский шаг (например, если согласовали по почте), оставив след в
		// step_transition.
		for i := curIdx; i < targetIdx; i++ {
			switch steps[i].Owner {
			case OwnerTeam, OwnerSystem:
				if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status='done', completed_at=COALESCE(completed_at,$2), updated_at=now()
WHERE id=$1 AND status IN ('pending','in_progress','rejected')`,
					steps[i].ID, now); err != nil {
					return Project{}, fmt.Errorf("close team step: %w", err)
				}
			case OwnerClient:
				if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status='skipped', completed_at=COALESCE(completed_at,$2), updated_at=now()
WHERE id=$1 AND status IN ('pending','in_progress','waiting_client','rejected')`,
					steps[i].ID, now); err != nil {
					return Project{}, fmt.Errorf("skip client step: %w", err)
				}
				// Лог-событие, чтобы было видно в фиде «менеджер пропустил клиента».
				if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, to_status, payload)
VALUES ($1, $2, $3, 'human', 'step_skipped_by_staff', 'skipped', $4)`,
					projectID, steps[i].ID, actorID,
					mustJSON(map[string]string{"reason": "manager_move"})); err != nil {
					return Project{}, fmt.Errorf("insert skip event: %w", err)
				}
			}
		}
	} else {
		// 6. Назад: сбрасываем шаги target..current в pending.
		for i := targetIdx; i <= curIdx; i++ {
			if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status='pending', started_at=NULL, completed_at=NULL,
    review_deadline=NULL, updated_at=now()
WHERE id=$1`, steps[i].ID); err != nil {
				return Project{}, fmt.Errorf("reset step: %w", err)
			}
		}
	}

	// 7. Активируем целевой шаг.
	newStatus := StepStatusInProgress
	if steps[targetIdx].Owner == OwnerClient {
		newStatus = StepStatusWaitingClient
	}
	var deadline *time.Time
	if steps[targetIdx].IsReview && newStatus == StepStatusWaitingClient && reviewDeadline > 0 {
		d := now.Add(reviewDeadline)
		deadline = &d
	}
	if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status=$2,
    started_at=COALESCE(started_at,$3),
    completed_at=NULL,
    review_deadline=COALESCE($4, review_deadline),
    updated_at=now()
WHERE id=$1`, steps[targetIdx].ID, string(newStatus), now, deadline); err != nil {
		return Project{}, fmt.Errorf("activate target step: %w", err)
	}

	// 8. Пересчёт completed_at для стадий: все шаги done|skipped → completed;
	// иначе NULL. Простой UPDATE по всему набору стадий проекта.
	if _, err := tx.Exec(ctx, `
UPDATE project_stages st
SET completed_at = CASE
    WHEN NOT EXISTS (
        SELECT 1 FROM project_steps ps
        WHERE ps.stage_id = st.id AND ps.status NOT IN ('done','skipped')
    ) THEN COALESCE(st.completed_at, $2)
    ELSE NULL
END,
    started_at = CASE
    WHEN EXISTS (
        SELECT 1 FROM project_steps ps
        WHERE ps.stage_id = st.id AND ps.status <> 'pending'
    ) THEN COALESCE(st.started_at, $2)
    ELSE NULL
END
WHERE st.project_id = $1`, projectID, now); err != nil {
		return Project{}, fmt.Errorf("recalc stages: %w", err)
	}

	// 9. Bump projects + optimistic lock.
	if err := r.bumpProject(ctx, tx, projectID, expectedUpdatedAt); err != nil {
		return Project{}, err
	}

	// Event: stage_moved (если стадия сменилась) + step transition.
	if fromStageID != toStageID {
		if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'stage_moved', $3)`,
			projectID, actorID,
			mustJSON(map[string]string{
				"from_stage_id": fromStageID.String(),
				"to_stage_id":   toStageID.String(),
			})); err != nil {
			return Project{}, fmt.Errorf("insert stage_moved: %w", err)
		}
		if err := emit(ctx, tx, projectID, nil, actorID, "project.stage_moved",
			map[string]string{
				"project_id":    projectID.String(),
				"from_stage_id": fromStageID.String(),
				"to_stage_id":   toStageID.String(),
			}); err != nil {
			return Project{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, to_status, payload)
VALUES ($1, $2, $3, 'human', 'step_moved', $4, $5)`,
		projectID, steps[targetIdx].ID, actorID, string(newStatus),
		mustJSON(map[string]string{
			"target_step_id": steps[targetIdx].ID.String(),
		})); err != nil {
		return Project{}, fmt.Errorf("insert step_moved: %w", err)
	}
	if err := emit(ctx, tx, projectID, &steps[targetIdx].ID, actorID, "project.step_moved",
		map[string]string{
			"project_id":     projectID.String(),
			"target_step_id": steps[targetIdx].ID.String(),
		}); err != nil {
		return Project{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetByID(ctx, projectID)
}

// bumpProject — атомарный bump projects.updated_at с optimistic-lock.
func (r *Repo) bumpProject(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, expectedUpdatedAt *time.Time) error {
	tag, err := tx.Exec(ctx, `
UPDATE projects SET updated_at = now() WHERE id = $1
   AND ($2::timestamptz IS NULL OR updated_at = $2)`,
		projectID, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("bump project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// необходим для совместимости — переэкспорт errors.Is
var _ = errors.Is
