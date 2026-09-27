package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/orders"
	"marketpclce/tests/integration"
)

// Приглашения уходят по приоритету, и передумать после «отправить» —
// нормальная просьба. Но переставлять можно только тех, кого ещё не звали.

func TestReorderReserve(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	// Одно место, четверо в подборке. Приглашения уходят отдельным шагом,
	// поэтому сразу после Create в резерве все четверо.
	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:4],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orderID := res.Order.ID

	// Зовём первого: у него начинает тикать срок ответа, и перестановка
	// его трогать не должна.
	order, err := svc.SendInvitations(ctx, orderID, now)
	if err != nil {
		t.Fatalf("SendInvitations: %v", err)
	}

	reserve := make([]uuid.UUID, 0, 3)
	var invited uuid.UUID
	for _, c := range order.Candidates {
		if c.Status == orders.CandidateInvited {
			invited = c.CreatorUserID
			continue
		}
		reserve = append(reserve, c.CreatorUserID)
	}
	if invited == uuid.Nil || len(reserve) != 3 {
		t.Fatalf("ожидался один приглашённый и трое в резерве: %+v", order.Candidates)
	}

	// Переставляем резерв задом наперёд.
	reversed := []uuid.UUID{reserve[2], reserve[1], reserve[0]}
	got, err := svc.ReorderReserve(ctx, orderID, clientID, reversed)
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}

	// Приглашённый на месте: у него уже тикает срок ответа.
	gotReserve := make([]uuid.UUID, 0, 3)
	for _, c := range got.Candidates {
		if c.CreatorUserID == invited {
			if c.Status != orders.CandidateInvited {
				t.Errorf("перестановка задела приглашённого: %+v", c)
			}
			continue
		}
		gotReserve = append(gotReserve, c.CreatorUserID)
	}
	for i := range reversed {
		if gotReserve[i] != reversed[i] {
			t.Fatalf("порядок не тот: want %v, got %v", reversed, gotReserve)
		}
	}
}

// Перестановка меняет порядок, а не состав.
func TestReorderRejectsWrongSet(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:3],
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orderID := res.Order.ID

	reserve := make([]uuid.UUID, 0, 2)
	for _, c := range res.Order.Candidates {
		if c.Status != orders.CandidateInvited {
			reserve = append(reserve, c.CreatorUserID)
		}
	}

	cases := map[string][]uuid.UUID{
		"неполный список": reserve[:1],
		"посторонний":     {reserve[0], uuid.New()},
		"дубль":           {reserve[0], reserve[0]},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.ReorderReserve(ctx, orderID, clientID, in); !errors.Is(err, orders.ErrPrioritySetMismatch) {
				t.Errorf("want ErrPrioritySetMismatch, got %v", err)
			}
		})
	}

	// Чужой заказ не переставляется.
	if _, err := svc.ReorderReserve(ctx, orderID, uuid.New(), reserve); !errors.Is(err, orders.ErrNotFound) {
		t.Errorf("чужой заказ: want ErrNotFound, got %v", err)
	}

	// Пустой список — не перестановка.
	if _, err := svc.ReorderReserve(ctx, orderID, clientID, nil); !errors.Is(err, orders.ErrInvalidInput) {
		t.Errorf("пустой список: want ErrInvalidInput, got %v", err)
	}
}

// Собранный заказ переставлять незачем.
func TestReorderLockedAfterStaffed(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:3],
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := integration.Pool(t).Exec(ctx,
		`UPDATE creator_orders SET status = 'staffed' WHERE id = $1`, res.Order.ID); err != nil {
		t.Fatalf("staff: %v", err)
	}
	reserve := make([]uuid.UUID, 0, 2)
	for _, c := range res.Order.Candidates {
		if c.Status != orders.CandidateInvited {
			reserve = append(reserve, c.CreatorUserID)
		}
	}
	if _, err := svc.ReorderReserve(ctx, res.Order.ID, clientID, reserve); !errors.Is(err, orders.ErrPriorityLocked) {
		t.Errorf("want ErrPriorityLocked, got %v", err)
	}
}

// ---- ТЕСТ: лимит считается на выбранный месяц ----

// Клиент берёт команду с конкретного месяца, и лимит должен считаться на
// него. Иначе интерфейс покажет одно число, а создание заказа откажет по
// другому.
func TestLimitDependsOnChosenMonth(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Месяцев работы нет — в первый месяц один креатор.
	allowed, err := svc.AllowedCreators(ctx, clientID, thisMonth)
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if allowed != orders.FirstMonthCreators {
		t.Fatalf("первый месяц: want %d, got %d", orders.FirstMonthCreators, allowed)
	}

	// Заводим оплаченный заказ на текущий месяц: к следующему он закрыт.
	// Версию правил берём действующую, а не свою: новая версия сбросила бы
	// согласие клиента, и заказ отвалился бы совсем по другой причине.
	var termsID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM terms_versions ORDER BY version DESC LIMIT 1`).Scan(&termsID); err != nil {
		t.Fatalf("terms: %v", err)
	}
	var orderID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO creator_orders (client_user_id, start_month, needed, videos_count,
                            terms_version_id, status, paid_at)
VALUES ($1, $2, 1, 30, $3, 'paid', now()) RETURNING id`,
		clientID, thisMonth, termsID).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM creator_orders WHERE id = $1`, orderID)

	// На текущий месяц он ещё не закрыт — по-прежнему один.
	allowed, err = svc.AllowedCreators(ctx, clientID, thisMonth)
	if err != nil {
		t.Fatalf("allowed 2: %v", err)
	}
	if allowed != orders.FirstMonthCreators {
		t.Errorf("незакрытый месяц не должен давать опыта: got %d", allowed)
	}

	// А на следующий — уже 2–3.
	next := thisMonth.AddDate(0, 1, 0)
	allowed, err = svc.AllowedCreators(ctx, clientID, next)
	if err != nil {
		t.Fatalf("allowed 3: %v", err)
	}
	if allowed != orders.MaxCreators {
		t.Errorf("со второго месяца: want %d, got %d", orders.MaxCreators, allowed)
	}

	completed, err := svc.CompletedMonths(ctx, clientID, next)
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if completed != 1 {
		t.Errorf("закрытых месяцев: want 1, got %d", completed)
	}

	// И заказ на следующий месяц на двоих проходит: лимит при создании
	// считается по месяцу заказа, а не по сегодняшнему дню.
	if _, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: next, Needed: 2,
		VideosCount: 60, CreatorIDs: creators[:3],
	}, now); err != nil {
		t.Errorf("заказ на второй месяц на двоих должен проходить: %v", err)
	}
}
