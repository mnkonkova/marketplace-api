package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidTransition — IsValidStepTransition вернул false.
// Хендлер мапит в 422 (а не 400), чтобы отличить «синтаксически
// невалидный запрос» от «легитимный запрос, но нельзя в этом состоянии».
var ErrInvalidTransition = errors.New("invalid step transition")

// transitionContext — параметры одного перехода шага. Используется
// внутренним хелпером, который применяется и admin-методами
// (Start/Complete/Skip), и клиентскими (Approve/RequestRevision).
type transitionContext struct {
	ProjectID         uuid.UUID
	StepID            uuid.UUID
	To                StepStatus
	ActorUserID       uuid.UUID
	ActorType         string // "human"/"service"/"system"
	Comment           string
	ExpectedUpdatedAt *time.Time // optimistic-lock на шаг; nil = не проверять
}

// stepRow — состояние шага, считанное под row-lock'ом.
type stepRow struct {
	ID                  uuid.UUID
	ProjectID           uuid.UUID
	StageID             uuid.UUID
	Code                string
	Owner               StepOwner
	Status              StepStatus
	DurationDays        int
	VisibleToClient     bool
	VisibleToSpecialist bool
	UpdatedAt           time.Time
}

// transitionStep — общий движок перехода:
//  1. SELECT шаг FOR UPDATE по project+step (так же сразу проверяем,
//     что шаг принадлежит проекту — защита от перебора UUID).
//  2. Optimistic-lock: если ExpectedUpdatedAt задан и не совпал — 409.
//  3. IsValidStepTransition(current, to).
//  4. UPDATE с проставлением started_at/completed_at и eta_date по правилам.
//  5. INSERT project_step_events (аудит-лог, обязательно).
//  6. emit step.transitioned в outbox (в той же tx).
//
// Возвращает обновлённый stepRow. Завершение стадии/проекта
// (cascade-эффекты) — отдельный helper finalizeIfTerminal, вызывается
// снаружи; чтобы не блокировать стадию через ещё один lock-проход здесь.
func (s *Service) transitionStep(ctx context.Context, tx pgx.Tx, tc transitionContext) (stepRow, error) {
	// 1. Считываем шаг под row-lock'ом. Дополнительно убеждаемся, что он
	// действительно принадлежит этому проекту.
	const lockQ = `
SELECT id, project_id, stage_id, code, owner, status,
       duration_days, visible_to_client, visible_to_specialist, updated_at
FROM project_steps
WHERE id = $1 AND project_id = $2
FOR UPDATE`
	var step stepRow
	var ownerStr string
	err := tx.QueryRow(ctx, lockQ, tc.StepID, tc.ProjectID).Scan(
		&step.ID, &step.ProjectID, &step.StageID, &step.Code, &ownerStr, &step.Status,
		&step.DurationDays, &step.VisibleToClient, &step.VisibleToSpecialist, &step.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return stepRow{}, ErrNotFound
	}
	if err != nil {
		return stepRow{}, fmt.Errorf("lock step: %w", err)
	}
	step.Owner = StepOwner(ownerStr)

	// 2. Optimistic-lock.
	if tc.ExpectedUpdatedAt != nil && !tc.ExpectedUpdatedAt.Equal(step.UpdatedAt) {
		return stepRow{}, ErrConflict
	}

	// Если уже целевой статус — идемпотентный no-op (защита от двойного
	// клика в Directus). Возвращаем текущее состояние без событий.
	if step.Status == tc.To {
		return step, nil
	}

	// 3. Валидация перехода.
	if !IsValidStepTransition(step.Status, tc.To) {
		return stepRow{}, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, step.Status, tc.To)
	}

	// 4. UPDATE. started_at проставляем при первом переходе в in_progress;
	// completed_at — при переходе в done/skipped; eta_date — только при
	// in_progress (после rejected → in_progress пересчитываем — клиент
	// видит новую дату).
	now := time.Now().UTC()
	var etaUpdate string
	args := []any{step.ID, tc.To}
	startedSet := ""
	completedSet := ""
	if tc.To == StepInProgress {
		// ETA = today + duration_days.
		etaDate := time.Now().UTC().AddDate(0, 0, step.DurationDays)
		args = append(args, etaDate)
		etaUpdate = fmt.Sprintf(", eta_date = $%d", len(args))
		// started_at: проставляем только если ещё nil.
		startedSet = ", started_at = COALESCE(started_at, now())"
	}
	if IsTerminalStep(tc.To) {
		completedSet = ", completed_at = now()"
	}
	updQ := fmt.Sprintf(`
UPDATE project_steps SET
    status = $2%s%s%s,
    updated_at = now()
WHERE id = $1
RETURNING updated_at`,
		etaUpdate, startedSet, completedSet)
	var newUpdatedAt time.Time
	if err := tx.QueryRow(ctx, updQ, args...).Scan(&newUpdatedAt); err != nil {
		return stepRow{}, fmt.Errorf("update step status: %w", err)
	}
	step.UpdatedAt = newUpdatedAt
	prev := step.Status
	step.Status = tc.To

	// 5. Аудит-лог.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events (project_id, step_id, actor_user_id, actor_type,
                                  from_status, to_status, comment)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))`,
		tc.ProjectID, step.ID, nullableUUID(tc.ActorUserID), tc.ActorType,
		prev, tc.To, tc.Comment,
	); err != nil {
		return stepRow{}, fmt.Errorf("insert event: %w", err)
	}

	// 6. Outbox-событие. Денормализуем адреса (best-effort).
	if err := s.emitStepTransitioned(ctx, tx, step, prev, tc.To, tc.ActorUserID, tc.ActorType, now); err != nil {
		return stepRow{}, err
	}
	return step, nil
}

// emitStepTransitioned — общий emit для шагов. Не использует
// s.emitCreatedInTx — там другой набор полей и логика. Хочется
// денормализовать клиента/специалиста/менеджера, поэтому сначала
// читаем проект (без блокировки — на уровне tx уже идём).
func (s *Service) emitStepTransitioned(
	ctx context.Context, tx pgx.Tx,
	step stepRow, from, to StepStatus,
	actorUserID uuid.UUID, actorType string,
	occurredAt time.Time,
) error {
	// Читаем поля проекта, нужные для payload. Без FOR UPDATE — уже
	// внутри tx после write'а в project_steps; целостность OK.
	var (
		projectSource ProjectSource
		title         string
		leadID        *uuid.UUID
		clientUserID  uuid.UUID
		specialistID  *uuid.UUID
		assignedID    *uuid.UUID
	)
	err := tx.QueryRow(ctx, `
SELECT source, title, lead_id, client_user_id, specialist_user_id, assigned_to_user_id
FROM projects WHERE id = $1`,
		step.ProjectID,
	).Scan(&projectSource, &title, &leadID, &clientUserID, &specialistID, &assignedID)
	if err != nil {
		return fmt.Errorf("read project for emit: %w", err)
	}

	payload := ProjectEventPayload{
		ProjectID:        step.ProjectID,
		ProjectSource:    projectSource,
		LeadID:           leadID,
		Title:            title,
		StepID:           &step.ID,
		StepCode:         step.Code,
		StepOwner:        step.Owner,
		FromStatus:       from,
		ToStatus:         to,
		ActorUserID:      maybeUUID(actorUserID),
		ActorType:        actorType,
		ClientUserID:     clientUserID,
		SpecialistUserID: specialistID,
		AssignedToUserID: assignedID,
		OccurredAt:       occurredAt,
	}
	if s.users != nil {
		if email, name, err := s.users.GetEmailAndName(ctx, clientUserID); err == nil {
			payload.ClientEmail, payload.ClientName = email, name
		}
		if specialistID != nil {
			if email, _, err := s.users.GetEmailAndName(ctx, *specialistID); err == nil {
				payload.SpecialistEmail = email
			}
		}
		if assignedID != nil {
			if email, _, err := s.users.GetEmailAndName(ctx, *assignedID); err == nil {
				payload.ManagerEmail = email
			}
		}
	}
	return emitProjectEvent(ctx, tx, EventStepTransitioned, payload)
}

// finalizeIfTerminal — после успешного перехода шага в done/skipped
// проверяет, не закрылась ли стадия (все шаги стадии теперь terminal)
// и не закрылся ли проект целиком. Эффекты:
//   - stage.completed_at = now()
//   - project.status = 'done', completed_at = now(), emit project.completed
//
// Вне tx этот вызов делать НЕЛЬЗЯ: project.status и stage.completed_at
// должны меняться атомарно с переходом шага.
func (s *Service) finalizeIfTerminal(ctx context.Context, tx pgx.Tx, step stepRow) error {
	if !IsTerminalStep(step.Status) {
		return nil
	}

	// Стадия: все ли её шаги в done/skipped?
	var stageOpen int
	if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM project_steps
WHERE stage_id = $1 AND status NOT IN ('done','skipped')`,
		step.StageID).Scan(&stageOpen); err != nil {
		return fmt.Errorf("count stage open steps: %w", err)
	}
	if stageOpen == 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE project_stages SET completed_at = COALESCE(completed_at, now()) WHERE id = $1`,
			step.StageID,
		); err != nil {
			return fmt.Errorf("close stage: %w", err)
		}
	}

	// Проект: все ли шаги в done/skipped?
	var projOpen int
	if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM project_steps
WHERE project_id = $1 AND status NOT IN ('done','skipped')`,
		step.ProjectID).Scan(&projOpen); err != nil {
		return fmt.Errorf("count project open steps: %w", err)
	}
	if projOpen != 0 {
		return nil
	}

	// Все шаги terminal — закрываем проект. Идемпотентность: если status
	// уже done, COALESCE сохранит существующий completed_at.
	if _, err := tx.Exec(ctx,
		`UPDATE projects SET status = 'done', completed_at = COALESCE(completed_at, now()), updated_at = now()
		 WHERE id = $1 AND status NOT IN ('done','cancelled')`,
		step.ProjectID,
	); err != nil {
		return fmt.Errorf("close project: %w", err)
	}

	// Project.completed-событие.
	payload, err := s.buildProjectPayload(ctx, tx, step.ProjectID)
	if err != nil {
		return err
	}
	payload.OccurredAt = time.Now().UTC()
	return emitProjectEvent(ctx, tx, EventProjectCompleted, payload)
}

// buildProjectPayload — небольшой helper для project-level событий
// (completed/disputed). Читает project + денормализует адреса.
func (s *Service) buildProjectPayload(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (ProjectEventPayload, error) {
	var (
		source       ProjectSource
		title        string
		leadID       *uuid.UUID
		clientUserID uuid.UUID
		specialistID *uuid.UUID
		assignedID   *uuid.UUID
	)
	err := tx.QueryRow(ctx, `
SELECT source, title, lead_id, client_user_id, specialist_user_id, assigned_to_user_id
FROM projects WHERE id = $1`, projectID,
	).Scan(&source, &title, &leadID, &clientUserID, &specialistID, &assignedID)
	if err != nil {
		return ProjectEventPayload{}, fmt.Errorf("read project: %w", err)
	}
	p := ProjectEventPayload{
		ProjectID:        projectID,
		ProjectSource:    source,
		LeadID:           leadID,
		Title:            title,
		ClientUserID:     clientUserID,
		SpecialistUserID: specialistID,
		AssignedToUserID: assignedID,
	}
	if s.users != nil {
		if email, name, err := s.users.GetEmailAndName(ctx, clientUserID); err == nil {
			p.ClientEmail, p.ClientName = email, name
		}
		if specialistID != nil {
			if email, _, err := s.users.GetEmailAndName(ctx, *specialistID); err == nil {
				p.SpecialistEmail = email
			}
		}
		if assignedID != nil {
			if email, _, err := s.users.GetEmailAndName(ctx, *assignedID); err == nil {
				p.ManagerEmail = email
			}
		}
	}
	return p, nil
}

// ─── Публичные admin-методы ─────────────────────────────────────────

// StartStep переводит шаг в in_progress. ETA пересчитывается синхронно.
func (s *Service) StartStep(ctx context.Context, projectID, stepID, actorUserID uuid.UUID, expectedUpdatedAt *time.Time) (StepView, error) {
	return s.runTransition(ctx, transitionContext{
		ProjectID:         projectID,
		StepID:            stepID,
		To:                StepInProgress,
		ActorUserID:       actorUserID,
		ActorType:         "human",
		ExpectedUpdatedAt: expectedUpdatedAt,
	})
}

// CompleteStep переводит шаг в done. Если это последний шаг стадии/проекта —
// закрывает их и эмитит project.completed.
func (s *Service) CompleteStep(ctx context.Context, projectID, stepID, actorUserID uuid.UUID, expectedUpdatedAt *time.Time) (StepView, error) {
	return s.runTransition(ctx, transitionContext{
		ProjectID:         projectID,
		StepID:            stepID,
		To:                StepDone,
		ActorUserID:       actorUserID,
		ActorType:         "human",
		ExpectedUpdatedAt: expectedUpdatedAt,
	})
}

// SkipStep — для опциональных шагов. comment рекомендуется (audit-лог).
func (s *Service) SkipStep(ctx context.Context, projectID, stepID, actorUserID uuid.UUID, comment string, expectedUpdatedAt *time.Time) (StepView, error) {
	return s.runTransition(ctx, transitionContext{
		ProjectID:         projectID,
		StepID:            stepID,
		To:                StepSkipped,
		ActorUserID:       actorUserID,
		ActorType:         "human",
		Comment:           comment,
		ExpectedUpdatedAt: expectedUpdatedAt,
	})
}

// runTransition — оборачивает transitionStep + finalizeIfTerminal в tx
// и возвращает свежую StepView для ответа.
func (s *Service) runTransition(ctx context.Context, tc transitionContext) (StepView, error) {
	var result StepView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		step, err := s.transitionStep(ctx, tx, tc)
		if err != nil {
			return err
		}
		if err := s.finalizeIfTerminal(ctx, tx, step); err != nil {
			return err
		}
		// Свежая view с актуальными полями.
		var ownerStr string
		if err := tx.QueryRow(ctx, `
SELECT id, code, title, owner, status, duration_days, weight, sort_order,
       eta_date, cta_payload, started_at, completed_at, updated_at
FROM project_steps WHERE id = $1`,
			step.ID,
		).Scan(
			&result.ID, &result.Code, &result.Title, &ownerStr, &result.Status,
			&result.DurationDays, &result.Weight, &result.SortOrder,
			&result.ETADate, &result.CTAPayload, &result.StartedAt, &result.CompletedAt, &result.UpdatedAt,
		); err != nil {
			return err
		}
		result.Owner = StepOwner(ownerStr)
		return nil
	})
	return result, err
}

// nullableUUID — превращает uuid.Nil в SQL NULL для actor_user_id.
// system/service-события не имеют конкретного юзера.
func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

func maybeUUID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}
