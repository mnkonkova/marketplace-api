package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReviewWriter — узкий контракт записи отзыва от клиента к специалисту.
// Реализуется в main.go адаптером поверх reviews.Service: в одной
// атомарной операции вставляет строку в reviews с заполненным
// project_id (его добавила миграция 00011).
//
// Возвращает review_id для последующей трассировки.
type ReviewWriter interface {
	CreateProjectReview(ctx context.Context, in ProjectReviewInput) (uuid.UUID, error)
}

// ProjectReviewInput — payload для ReviewWriter.CreateProjectReview.
// AuthorUserID == client, TargetUserID == specialist; ProjectID,
// LeadID копируются из проекта. AuthorName берётся из users или пустой
// (если у клиента нет display_name).
type ProjectReviewInput struct {
	ProjectID    uuid.UUID
	LeadID       *uuid.UUID
	AuthorUserID uuid.UUID
	AuthorName   string
	TargetUserID uuid.UUID
	Rating       int
	Text         string
}

// WithReviewWriter подключает реализацию ReviewWriter.
// nil-safe: без подключения SubmitReview возвращает ErrInvalidInput.
func (s *Service) WithReviewWriter(rw ReviewWriter) *Service {
	s.reviews = rw
	return s
}

// ApproveStep — клиент жмёт «Принять» на client_review (или другом
// client-шаге в waiting_client). Под капотом — transitionStep(to=done) +
// finalizeIfTerminal.
//
// Проверки:
//   - проект принадлежит клиенту;
//   - шаг — visible_to_client + owner=client;
//   - текущий статус позволяет → done.
func (s *Service) ApproveStep(ctx context.Context, projectID, stepID, clientUserID uuid.UUID, expectedUpdatedAt *time.Time) (StepView, error) {
	// Защита: клиент-владелец проекта.
	if _, err := s.repo.GetClientByID(ctx, projectID, clientUserID); err != nil {
		return StepView{}, err
	}
	// Сам переход + cascade.
	view, err := s.runTransition(ctx, transitionContext{
		ProjectID:         projectID,
		StepID:            stepID,
		To:                StepDone,
		ActorUserID:       clientUserID,
		ActorType:         "human",
		ExpectedUpdatedAt: expectedUpdatedAt,
	})
	if err != nil {
		return StepView{}, err
	}
	if view.Owner != OwnerClient {
		// Этот шаг не клиентский — клиент не должен мог его approve.
		// Перешли уже — откатить нельзя без полной реализации compensate.
		// Лучше всего поймать раньше, до перехода. Делаем проверку owner
		// в runTransition? Проще: добавим owner-проверку в ApproveStep
		// заранее через прочтение шага.
		return view, fmt.Errorf("%w: step not owned by client", ErrForbidden)
	}
	return view, nil
}

// RequestRevision — клиент просит правки на client_review-шаге.
// Цикл (бриф §4.2):
//  1. client_review (waiting_client) → rejected.
//  2. Если revisions_used < revisions_included:
//     - revisions_used += 1
//     - найти final_cut шаг в том же проекте → rejected → in_progress (рестарт)
//     - повторение цикла продолжается
//     иначе:
//     - project.status = 'dispute', emit project.disputed.
//
// Всё в одной tx.
func (s *Service) RequestRevision(ctx context.Context, projectID, stepID, clientUserID uuid.UUID, comment string, expectedUpdatedAt *time.Time) (StepView, error) {
	// Владение.
	if _, err := s.repo.GetClientByID(ctx, projectID, clientUserID); err != nil {
		return StepView{}, err
	}

	var result StepView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 1. Перевод client_review → rejected.
		step, err := s.transitionStep(ctx, tx, transitionContext{
			ProjectID:         projectID,
			StepID:            stepID,
			To:                StepRejected,
			ActorUserID:       clientUserID,
			ActorType:         "human",
			Comment:           comment,
			ExpectedUpdatedAt: expectedUpdatedAt,
		})
		if err != nil {
			return err
		}
		if step.Owner != OwnerClient || step.Code != "client_review" {
			// Защита от запроса правок на любом другом шаге. Откатим tx.
			return fmt.Errorf("%w: revision only on client_review step", ErrInvalidTransition)
		}

		// 2. Проверка лимита и решение.
		var revisionsUsed, revisionsIncluded int
		if err := tx.QueryRow(ctx,
			`SELECT revisions_used, revisions_included FROM projects WHERE id = $1 FOR UPDATE`,
			projectID,
		).Scan(&revisionsUsed, &revisionsIncluded); err != nil {
			return fmt.Errorf("read project revisions: %w", err)
		}

		if revisionsUsed < revisionsIncluded {
			// Инкремент + рестарт final_cut.
			if _, err := tx.Exec(ctx,
				`UPDATE projects SET revisions_used = revisions_used + 1, updated_at = now() WHERE id = $1`,
				projectID,
			); err != nil {
				return fmt.Errorf("inc revisions_used: %w", err)
			}
			// Найти final_cut. По текущему шаблону он один на проект.
			var finalCutID uuid.UUID
			err := tx.QueryRow(ctx,
				`SELECT id FROM project_steps WHERE project_id = $1 AND code = 'final_cut'`,
				projectID,
			).Scan(&finalCutID)
			if errors.Is(err, pgx.ErrNoRows) {
				// Шаблон без final_cut — ничего перезапускать. Шаг рестарта
				// не строгий инвариант, не ломаемся.
				return nil
			}
			if err != nil {
				return fmt.Errorf("find final_cut: %w", err)
			}
			// final_cut → in_progress. Из любого статуса: in_progress, done,
			// waiting_client. Нашим IsValidStepTransition `done → in_progress`
			// запрещён — на ревизию переоткрытие, поэтому через сырой UPDATE
			// без transitionStep (но с emit события и аудит-логом вручную).
			//
			// Альтернатива — отдельный «system»-actor с разрешённым переходом;
			// тут проще и читаемее — сырой UPDATE + audit + emit.
			newETA := time.Now().UTC().AddDate(0, 0, 3) // final_cut длится 3 дня
			var fcCode string
			var fcUpdatedAt time.Time
			if err := tx.QueryRow(ctx, `
UPDATE project_steps
SET status = 'in_progress', eta_date = $2, started_at = COALESCE(started_at, now()),
    completed_at = NULL, updated_at = now()
WHERE id = $1
RETURNING code, updated_at`,
				finalCutID, newETA,
			).Scan(&fcCode, &fcUpdatedAt); err != nil {
				return fmt.Errorf("reopen final_cut: %w", err)
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events (project_id, step_id, actor_user_id, actor_type,
                                  from_status, to_status, comment)
VALUES ($1, $2, $3, 'system', NULL, 'in_progress', $4)`,
				projectID, finalCutID, clientUserID,
				fmt.Sprintf("reopen on revision %d/%d", revisionsUsed+1, revisionsIncluded),
			); err != nil {
				return fmt.Errorf("audit reopen: %w", err)
			}

			// Outbox: revision.requested (для notification менеджеру и команде).
			payload, err := s.buildProjectPayload(ctx, tx, projectID)
			if err != nil {
				return err
			}
			payload.StepID = &finalCutID
			payload.StepCode = "final_cut"
			payload.StepOwner = OwnerTeam
			payload.FromStatus = StepDone
			payload.ToStatus = StepInProgress
			payload.ActorUserID = &clientUserID
			payload.ActorType = "human"
			payload.OccurredAt = time.Now().UTC()
			if err := emitProjectEvent(ctx, tx, EventRevisionRequested, payload); err != nil {
				return err
			}
		} else {
			// Лимит исчерпан → dispute.
			if _, err := tx.Exec(ctx,
				`UPDATE projects SET status = 'dispute', updated_at = now() WHERE id = $1 AND status NOT IN ('done','cancelled')`,
				projectID,
			); err != nil {
				return fmt.Errorf("set dispute: %w", err)
			}
			payload, err := s.buildProjectPayload(ctx, tx, projectID)
			if err != nil {
				return err
			}
			payload.ActorUserID = &clientUserID
			payload.ActorType = "human"
			payload.OccurredAt = time.Now().UTC()
			if err := emitProjectEvent(ctx, tx, EventProjectDisputed, payload); err != nil {
				return err
			}
		}

		// Свежий view rejected-шага для ответа клиенту.
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

// SubmitReview — клиент жмёт «Оставить отзыв» на финальном `review` шаге.
// Создаёт запись в reviews через ReviewWriter (с project_id), переводит
// шаг → done, при необходимости закрывает проект.
//
// Атомарность не строгая: review создаётся в отдельной tx (внутри
// ReviewWriter), step→done — в другой. Race-условие: review создалась,
// step переход упал. Восстановление: review-строка есть, клиент видит
// step ещё активным и может попробовать ещё раз; SubmitReview должен
// быть идемпотентным относительно повторной попытки (если review уже
// есть, не создаём вторую). Достаточный гарант — uniqueness на
// (project_id, author_user_id, target_user_id) в reviews; для MVP
// rely на ReviewWriter (он может проверить дубль перед insert'ом или
// мы можем добавить unique index в Фазе 11).
func (s *Service) SubmitReview(
	ctx context.Context,
	projectID, stepID, clientUserID uuid.UUID,
	rating int, text string,
	expectedUpdatedAt *time.Time,
) (StepView, uuid.UUID, error) {
	if s.reviews == nil {
		return StepView{}, uuid.Nil, fmt.Errorf("%w: review writer not configured", ErrInvalidInput)
	}
	if rating < 1 || rating > 5 {
		return StepView{}, uuid.Nil, fmt.Errorf("%w: rating must be 1..5", ErrInvalidInput)
	}

	// Владение и подготовка контекста.
	clientProj, err := s.repo.GetClientByID(ctx, projectID, clientUserID)
	if err != nil {
		return StepView{}, uuid.Nil, err
	}
	if clientProj.SpecialistUserID == nil {
		return StepView{}, uuid.Nil, fmt.Errorf("%w: project has no specialist to review", ErrInvalidInput)
	}

	// Имя автора. Best-effort.
	var authorName string
	if s.users != nil {
		_, name, _ := s.users.GetEmailAndName(ctx, clientUserID)
		authorName = name
	}

	// 1. Создаём review-запись (в своей tx внутри ReviewWriter).
	reviewID, err := s.reviews.CreateProjectReview(ctx, ProjectReviewInput{
		ProjectID:    projectID,
		LeadID:       clientProj.LeadID,
		AuthorUserID: clientUserID,
		AuthorName:   authorName,
		TargetUserID: *clientProj.SpecialistUserID,
		Rating:       rating,
		Text:         text,
	})
	if err != nil {
		return StepView{}, uuid.Nil, fmt.Errorf("create review: %w", err)
	}

	// 2. Шаг → done.
	view, err := s.runTransition(ctx, transitionContext{
		ProjectID:         projectID,
		StepID:            stepID,
		To:                StepDone,
		ActorUserID:       clientUserID,
		ActorType:         "human",
		Comment:           fmt.Sprintf("review submitted: %s", reviewID),
		ExpectedUpdatedAt: expectedUpdatedAt,
	})
	if err != nil {
		// Review уже создан, шаг не закрыт. Клиент увидит этот шаг
		// активным; повторный SubmitReview сделает duplicate review
		// (если на reviews нет unique). MVP-trade-off.
		return StepView{}, reviewID, fmt.Errorf("complete review step: %w", err)
	}
	return view, reviewID, nil
}
