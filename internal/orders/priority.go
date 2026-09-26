package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Перестановка приоритета в уже заведённом заказе.
//
// Клиент присылает не список, а ПОРЯДОК: приглашения уходят по нему, и
// передумать после «отправить» — нормальная просьба. Переставлять можно
// только тех, кого ещё не звали: у приглашённого уже тикает срок ответа,
// а у отказавшегося порядок не имеет смысла.

var (
	// ErrPriorityLocked — заказ в состоянии, где порядок уже не важен.
	ErrPriorityLocked = errors.New("order priority is no longer editable")
	// ErrPrioritySetMismatch — прислан не тот набор людей. Перестановка
	// меняет порядок, а не состав: добавление и удаление — это другие
	// операции, со своими проверками лимита и занятости.
	ErrPrioritySetMismatch = errors.New("reorder must list exactly the untouched reserve")
)

// ReorderReserve — переставить неприглашённых кандидатов в заданном порядке.
func (r *Repo) ReorderReserve(ctx context.Context, orderID, clientID uuid.UUID, order []uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM creator_orders WHERE id = $1 AND client_user_id = $2 FOR UPDATE`,
		orderID, clientID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock order: %w", err)
	}
	// Собранный, оплаченный и отменённый заказ переставлять незачем.
	// submitted — заявка из воронки: приглашений ещё не было, и порядок
	// строк в ней менять можно.
	if status != string(StatusDraft) && status != string(StatusSubmitted) &&
		status != string(StatusInviting) {
		return ErrPriorityLocked
	}

	// Текущий резерв — и он же тот набор, который обязан прийти.
	rows, err := tx.Query(ctx, `
SELECT creator_user_id, priority FROM order_candidates
WHERE order_id = $1 AND status = 'reserve'
ORDER BY priority`, orderID)
	if err != nil {
		return fmt.Errorf("load reserve: %w", err)
	}
	current := make(map[uuid.UUID]bool)
	slots := make([]int, 0, len(order))
	for rows.Next() {
		var id uuid.UUID
		var prio int
		if err := rows.Scan(&id, &prio); err != nil {
			rows.Close()
			return fmt.Errorf("scan reserve: %w", err)
		}
		current[id] = true
		slots = append(slots, prio)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(order) != len(current) {
		return ErrPrioritySetMismatch
	}
	seen := make(map[uuid.UUID]bool, len(order))
	for _, id := range order {
		if !current[id] || seen[id] {
			return ErrPrioritySetMismatch
		}
		seen[id] = true
	}

	// Занятые места достаются тем же людям, просто в новом порядке: так
	// перестановка не задевает уже приглашённых, стоящих между ними.
	//
	// В два прохода из-за уникального индекса (order_id, priority):
	// прямое присваивание ломается на первом же обмене двух соседей.
	// Сначала уводим в отрицательные номера — CHECK priority > 0 туда
	// не пускает, поэтому вычитаем из уже занятого места большое число,
	// оставаясь положительными.
	const parkOffset = 1000000
	for _, id := range order {
		if _, err := tx.Exec(ctx,
			`UPDATE order_candidates SET priority = priority + $3
			 WHERE order_id = $1 AND creator_user_id = $2`,
			orderID, id, parkOffset); err != nil {
			return fmt.Errorf("park priority: %w", err)
		}
	}
	for i, id := range order {
		if _, err := tx.Exec(ctx,
			`UPDATE order_candidates SET priority = $3
			 WHERE order_id = $1 AND creator_user_id = $2`,
			orderID, id, slots[i]); err != nil {
			return fmt.Errorf("set priority: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// ReorderReserve — переставить резерв заказа.
func (s *Service) ReorderReserve(ctx context.Context, orderID, clientID uuid.UUID, order []uuid.UUID) (Order, error) {
	if len(order) == 0 {
		return Order{}, fmt.Errorf("%w: empty order", ErrInvalidInput)
	}
	if err := s.repo.ReorderReserve(ctx, orderID, clientID, order); err != nil {
		return Order{}, err
	}
	return s.repo.Get(ctx, orderID)
}
