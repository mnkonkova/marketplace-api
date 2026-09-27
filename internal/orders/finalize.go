package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Финализация заявки менеджером — шаг 8 сценария «под ключ».
//
// Это не «создать», а «доукомплектовать»: проект заведён вместе с
// заявкой, чеклист прицепился сам, условия оплаты стоят по умолчанию.
// Остаётся то, что решается только человеком и только по телефону:
// кого берём, сколько роликов в месяце и на какую цену договорились.
//
// Цена и даты правятся своими ручками проекта — их менеджер и так
// открывает. Здесь только состав и объём: именно они превращают заявку
// в работу, и именно на них завязано письмо креатору с договором и ТЗ.

// ErrNoProject — у заявки нет проекта. Такое бывает только у строк,
// заведённых до 26.09.2026: с тех пор проект создаётся вместе с
// заявкой. Добавлять людей некуда, и молчать об этом нельзя.
var ErrNoProject = errors.New("order has no project")

// FinalizeResult — что получилось.
type FinalizeResult struct {
	Order Order `json:"order"`
	// Added — кого добавили в состав этим вызовом. Уже стоявшие в
	// составе сюда не попадают: повторная финализация не должна
	// выглядеть как второй набор людей.
	Added []uuid.UUID `json:"added"`
}

// Finalize — утвердить состав и объём.
//
// Людей добавляет CrewKeeper — тот же путь, которым менеджер добавляет
// креатора руками: вместе с составом человеку уходит задание проекта
// (договор из материалов, ТЗ и чеклист). Своей вставкой в
// project_creators здесь обойтись нельзя — уведомление осталось бы за
// бортом, и первый добавленный узнал бы о работе, открыв вкладку.
func (s *Service) Finalize(
	ctx context.Context, orderID uuid.UUID, creatorIDs []uuid.UUID,
	monthlyPlan int, actor uuid.UUID, now time.Time,
) (FinalizeResult, error) {
	var out FinalizeResult

	o, err := s.repo.Get(ctx, orderID)
	if err != nil {
		return out, err
	}
	if o.Status == StatusCancelled {
		return out, ErrWrongStatus
	}
	if o.ProjectID == nil {
		return out, ErrNoProject
	}

	seen := make(map[uuid.UUID]bool, len(creatorIDs))
	for _, id := range creatorIDs {
		if id == uuid.Nil {
			return out, fmt.Errorf("%w: пустой id креатора", ErrInvalidInput)
		}
		if seen[id] {
			return out, fmt.Errorf("%w: %s", ErrDuplicateCandidate, id)
		}
		seen[id] = true
	}

	if len(creatorIDs) > 0 && s.crew == nil {
		return out, errors.New("crew keeper is not configured")
	}
	for _, id := range creatorIDs {
		if err := s.crew.AddCreator(ctx, *o.ProjectID, id, actor); err != nil {
			// Частичный состав — это не беда: добавленные уже в
			// проекте и уже получили задание. Беда — молча сделать вид,
			// что добавились все.
			return out, fmt.Errorf("add creator %s: %w", id, err)
		}
		out.Added = append(out.Added, id)
	}

	// План месяца — в проект, а не в заказ: заказ хранит то, что
	// просили, проект — то, о чём договорились.
	if monthlyPlan > 0 && s.projects != nil {
		if err := s.projects.SetMonthlyPlan(ctx, *o.ProjectID, monthlyPlan); err != nil {
			return out, fmt.Errorf("set monthly plan: %w", err)
		}
	}

	order, err := s.repo.Finalize(ctx, orderID, creatorIDs, monthlyPlan, now)
	if err != nil {
		return out, err
	}
	out.Order = order
	return out, nil
}

// Finalize — перевести заявку в finalized и отметить взятых.
func (r *Repo) Finalize(
	ctx context.Context, orderID uuid.UUID, creatorIDs []uuid.UUID,
	monthlyPlan int, now time.Time,
) (Order, error) {
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var projectID *uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT project_id FROM creator_orders WHERE id = $1 FOR UPDATE`,
			orderID).Scan(&projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}

		// Взятые в состав — accepted. Строка кандидата у них уже есть:
		// либо заказчик отметил, либо человек откликнулся, либо
		// рассылка завела её сама. На всякий случай заводим — менеджер
		// имеет право взять кого угодно, в том числе мимо заявки.
		if len(creatorIDs) > 0 {
			if _, err := tx.Exec(ctx, `
INSERT INTO order_candidates (order_id, creator_user_id, priority, status, responded_at)
SELECT $1, c,
       (SELECT COALESCE(MAX(priority), 0) FROM order_candidates WHERE order_id = $1) + n,
       'accepted', $3
FROM unnest($2::uuid[]) WITH ORDINALITY AS t(c, n)
ON CONFLICT (order_id, creator_user_id) DO UPDATE
   SET status = 'accepted',
       responded_at = COALESCE(order_candidates.responded_at, EXCLUDED.responded_at)`,
				orderID, creatorIDs, now); err != nil {
				return fmt.Errorf("accept candidates: %w", err)
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE creator_orders SET status = 'finalized', updated_at = $2 WHERE id = $1`,
			orderID, now); err != nil {
			return fmt.Errorf("finalize order: %w", err)
		}

		payload := map[string]any{
			"order_id": orderID,
			"creators": len(creatorIDs),
		}
		if monthlyPlan > 0 {
			payload["monthly_plan"] = monthlyPlan
		}
		withProject(payload, projectID)
		return outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(),
			outbox.EventOrderFinalized, payload)
	})
	if err != nil {
		return Order{}, err
	}
	return r.Get(ctx, orderID)
}
