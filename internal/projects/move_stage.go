package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MoveProjectToStage — перевести проект на конкретную стадию (любую, не
// только +1 от текущей). Защита: при движении вперёд в промежуточных
// стадиях не должно быть незавершённых client-шагов; при движении назад
// стадии target..current сбрасываются в pending. Эмитит событие
// `project.stage_moved` в outbox — hook под будущие n8n-колбэки.
//
// reviewDeadline — длительность дедлайна для is_review+client шага.
// expectedUpdatedAt — optimistic-lock версии projects.updated_at; nil = без
// проверки. Защита от двух менеджеров двигающих один проект параллельно.
func (r *Repo) MoveProjectToStage(ctx context.Context, projectID, targetStageID, actorID uuid.UUID, reviewDeadline time.Duration, expectedUpdatedAt *time.Time) (Project, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Project{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
SELECT id, sort_order FROM project_stages
WHERE project_id = $1
ORDER BY sort_order
FOR UPDATE`, projectID)
	if err != nil {
		return Project{}, fmt.Errorf("lock stages: %w", err)
	}
	type st struct {
		ID    uuid.UUID
		Order int
	}
	var stages []st
	for rows.Next() {
		var s st
		if err := rows.Scan(&s.ID, &s.Order); err != nil {
			rows.Close()
			return Project{}, fmt.Errorf("scan stage: %w", err)
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

	targetIdx := -1
	for i, s := range stages {
		if s.ID == targetStageID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		// Возможно нам передали pipeline_stages.id (из канбана UI) вместо
		// project_stages.id (snapshot). Резолвим через sort_order: достаём
		// sort_order у pipeline-стадии и матчим с project_stages этого
		// проекта по тому же order.
		var pipelineOrder int
		err := tx.QueryRow(ctx,
			`SELECT sort_order FROM pipeline_stages WHERE id = $1`,
			targetStageID).Scan(&pipelineOrder)
		if err == nil {
			for i, s := range stages {
				if s.Order == pipelineOrder {
					targetIdx = i
					break
				}
			}
		}
	}
	if targetIdx == -1 {
		return Project{}, ErrNotFound
	}

	curIdx := -1
	for i, s := range stages {
		var allDone bool
		if err := tx.QueryRow(ctx, `
SELECT BOOL_AND(status IN ('done','skipped'))
FROM project_steps WHERE stage_id = $1`, s.ID).Scan(&allDone); err != nil {
			return Project{}, fmt.Errorf("check stage done: %w", err)
		}
		if !allDone {
			curIdx = i
			break
		}
	}
	// Все шаги done — берём последнюю стадию как «current».
	if curIdx == -1 {
		curIdx = len(stages) - 1
	}

	now := time.Now()

	// no-op
	if targetIdx == curIdx {
		if err := tx.Commit(ctx); err != nil {
			return Project{}, fmt.Errorf("commit: %w", err)
		}
		return r.GetByID(ctx, projectID)
	}

	if targetIdx > curIdx {
		// Движение ВПЕРЁД. Проверка клиентских шагов в стадиях [curIdx..targetIdx-1].
		for i := curIdx; i < targetIdx; i++ {
			var hasClientPending bool
			if err := tx.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM project_steps
              WHERE stage_id = $1 AND owner = 'client'
                AND status IN ('pending','in_progress','waiting_client','rejected'))`,
				stages[i].ID).Scan(&hasClientPending); err != nil {
				return Project{}, fmt.Errorf("check client pending: %w", err)
			}
			if hasClientPending {
				return Project{}, ErrStageBlocked
			}
		}
		for i := curIdx; i < targetIdx; i++ {
			if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status='done', completed_at=COALESCE(completed_at, $2), updated_at=now()
WHERE stage_id = $1 AND owner IN ('team','system')
  AND status IN ('pending','in_progress','rejected')`, stages[i].ID, now); err != nil {
				return Project{}, fmt.Errorf("complete team steps: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE project_stages SET completed_at = COALESCE(completed_at, $2) WHERE id = $1`,
				stages[i].ID, now); err != nil {
				return Project{}, fmt.Errorf("complete stage: %w", err)
			}
		}
	} else {
		// Движение НАЗАД. Сбрасываем шаги target..current обратно в pending.
		for i := targetIdx; i <= curIdx; i++ {
			if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status='pending', started_at=NULL, completed_at=NULL,
    review_deadline=NULL, updated_at=now()
WHERE stage_id = $1`, stages[i].ID); err != nil {
				return Project{}, fmt.Errorf("reset steps: %w", err)
			}
			// Стадиям > target обнуляем started/completed (target оставим — ей
			// выставим started ниже).
			if i > targetIdx {
				if _, err := tx.Exec(ctx,
					`UPDATE project_stages SET started_at=NULL, completed_at=NULL WHERE id = $1`,
					stages[i].ID); err != nil {
					return Project{}, fmt.Errorf("reset stage: %w", err)
				}
			}
		}
	}

	// Активируем первый шаг target-стадии.
	var firstStepID uuid.UUID
	var firstStepOwner string
	var firstStepIsReview bool
	err = tx.QueryRow(ctx, `
SELECT id, owner, is_review FROM project_steps
WHERE stage_id = $1 ORDER BY sort_order LIMIT 1`,
		stages[targetIdx].ID).Scan(&firstStepID, &firstStepOwner, &firstStepIsReview)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Project{}, fmt.Errorf("first step: %w", err)
	}
	if err == nil {
		newStatus := StepStatusInProgress
		if firstStepOwner == OwnerClient {
			newStatus = StepStatusWaitingClient
		}
		var deadline *time.Time
		if firstStepIsReview && newStatus == StepStatusWaitingClient && reviewDeadline > 0 {
			d := now.Add(reviewDeadline)
			deadline = &d
		}
		if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status = $2,
    started_at = COALESCE(started_at, $3),
    review_deadline = COALESCE($4, review_deadline),
    updated_at = now()
WHERE id = $1 AND status = 'pending'`,
			firstStepID, string(newStatus), now, deadline); err != nil {
			return Project{}, fmt.Errorf("activate first step: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE project_stages SET started_at = COALESCE(started_at, $2),
		                            completed_at = NULL
		 WHERE id = $1`,
		stages[targetIdx].ID, now); err != nil {
		return Project{}, fmt.Errorf("start target stage: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE projects SET updated_at = now() WHERE id = $1
		   AND ($2::timestamptz IS NULL OR updated_at = $2)`,
		projectID, expectedUpdatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("bump project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Project{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'stage_moved', $3)`,
		projectID, actorID,
		mustJSON(map[string]string{
			"from_stage_id": stages[curIdx].ID.String(),
			"to_stage_id":   stages[targetIdx].ID.String(),
		})); err != nil {
		return Project{}, fmt.Errorf("insert stage_moved event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.stage_moved",
		map[string]string{
			"project_id":    projectID.String(),
			"from_stage_id": stages[curIdx].ID.String(),
			"to_stage_id":   stages[targetIdx].ID.String(),
		},
	); err != nil {
		return Project{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetByID(ctx, projectID)
}
