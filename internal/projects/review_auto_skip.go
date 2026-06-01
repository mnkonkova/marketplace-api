package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExpiredReview — кандидат на авто-skip: review-шаг в waiting_client,
// дедлайн которого истёк. Возвращается из ListExpiredReviewSteps.
type ExpiredReview struct {
	ProjectID uuid.UUID
	StepID    uuid.UUID
}

// ListExpiredReviewSteps — выбирает все is_review=TRUE waiting_client шаги
// с review_deadline < now(). Узкий частичный индекс
// `project_steps_review_idx` делает запрос дешёвым даже на больших объёмах.
func (r *Repo) ListExpiredReviewSteps(ctx context.Context, now time.Time) ([]ExpiredReview, error) {
	rows, err := r.db.Query(ctx, `
SELECT project_id, id FROM project_steps
WHERE is_review = TRUE
  AND status = 'waiting_client'
  AND review_deadline IS NOT NULL
  AND review_deadline < $1`, now)
	if err != nil {
		return nil, fmt.Errorf("list expired reviews: %w", err)
	}
	defer rows.Close()
	out := make([]ExpiredReview, 0)
	for rows.Next() {
		var x ExpiredReview
		if err := rows.Scan(&x.ProjectID, &x.StepID); err != nil {
			return nil, fmt.Errorf("scan expired review: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AutoSkipReviewStep — переводит конкретный review-шаг в skipped (как от
// системного актора). Если после этого все шаги проекта в (done|skipped) —
// проект закрывается (status=done, completed_at=now). Идемпотентен: если
// шаг уже не waiting_client, transition вернёт ErrInvalidTransition, что
// caller считает «уже не нужно» (skipped из-за гонки worker'а в кластере).
func (r *Repo) AutoSkipReviewStep(ctx context.Context, projectID, stepID uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now()
	tag, err := tx.Exec(ctx, `
UPDATE project_steps
SET status = 'skipped', completed_at = $3, updated_at = now()
WHERE id = $1 AND project_id = $2 AND status = 'waiting_client' AND is_review = TRUE`,
		stepID, projectID, now)
	if err != nil {
		return fmt.Errorf("auto-skip update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidTransition
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, from_status, to_status, comment)
VALUES ($1, $2, NULL, 'system', 'step_transition', 'waiting_client', 'skipped', 'Автопропуск review-шага по дедлайну')`,
		projectID, stepID); err != nil {
		return fmt.Errorf("insert auto-skip event: %w", err)
	}

	// Проверяем: все ли шаги проекта в (done|skipped)? Если да — закрываем
	// проект (status=done, completed_at=now). Стадии тоже помечаем completed.
	var allDone bool
	if err := tx.QueryRow(ctx,
		`SELECT BOOL_AND(status IN ('done','skipped')) FROM project_steps WHERE project_id = $1`,
		projectID).Scan(&allDone); err != nil {
		return fmt.Errorf("check all steps done: %w", err)
	}
	if allDone {
		if _, err := tx.Exec(ctx, `
UPDATE projects SET status = 'done', completed_at = $2, updated_at = now()
WHERE id = $1 AND status NOT IN ('done','cancelled')`, projectID, now); err != nil {
			return fmt.Errorf("close project: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE project_stages SET completed_at = COALESCE(completed_at, $2)
WHERE project_id = $1`, projectID, now); err != nil {
			return fmt.Errorf("close stages: %w", err)
		}
		if err := emit(ctx, tx, projectID, nil, uuid.Nil, "project.completed",
			map[string]string{"project_id": projectID.String(), "reason": "review_auto_skipped"},
		); err != nil {
			return err
		}
	}

	if err := emit(ctx, tx, projectID, &stepID, uuid.Nil, "project.step_transitioned",
		map[string]any{
			"project_id": projectID.String(),
			"step_id":    stepID.String(),
			"from":       "waiting_client",
			"to":         "skipped",
			"actor_type": "system",
		},
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// RunReviewAutoSkip — пробежать по просроченным review-шагам и попытаться
// заскипать каждый. Возвращает (skipped, errors-count). Не падает на отдельной
// ошибке — продолжает обрабатывать остальные.
func (s *Service) RunReviewAutoSkip(ctx context.Context) (skipped, failed int, err error) {
	items, err := s.repo.ListExpiredReviewSteps(ctx, time.Now())
	if err != nil {
		return 0, 0, err
	}
	for _, x := range items {
		if err := s.repo.AutoSkipReviewStep(ctx, x.ProjectID, x.StepID); err != nil {
			if errors.Is(err, ErrInvalidTransition) {
				// гонка — другой worker уже скипнул. Не считаем ошибкой.
				continue
			}
			failed++
			continue
		}
		skipped++
	}
	return skipped, failed, nil
}
