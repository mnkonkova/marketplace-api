package integration_test

import (
	"context"
	"testing"
	"time"

	"marketpclce/internal/billing"
	"marketpclce/tests/integration"
)

// Менеджер подтверждает, каким числом кончается период.
//
// Автомат считает границу месяцем от первой выкладки и остаётся главным
// путём. Но подтверждённая дата сильнее вычисленной: план знает человек.
// А вместе с границей едет и цепочка — следующий период начинается со
// дня после подтверждённого конца.
func TestConfirmPeriodEndMovesChain(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	repo := billing.NewRepo(pool)
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "cfm01", 0)
	now := first.AddDate(0, 2, 0)

	p1, err := svc.Period(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("период 1: %v", err)
	}
	// Автомат: 5 апреля — 4 мая.
	if got := p1.EndsOn.Format("2006-01-02"); got != "2026-05-04" {
		t.Fatalf("вычисленный конец %s, ожидался 2026-05-04", got)
	}
	p2, err := svc.Period(ctx, pid, 2, now)
	if err != nil {
		t.Fatalf("период 2: %v", err)
	}
	if got := p2.StartsOn.Format("2006-01-02"); got != "2026-05-05" {
		t.Fatalf("начало периода 2 %s, ожидалось 2026-05-05", got)
	}

	// Менеджер говорит: последняя выкладка 30 апреля, период кончается им.
	confirmed, err := repo.ConfirmPeriodEnd(ctx, p1.ID,
		time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), creators[0], now)
	if err != nil {
		t.Fatalf("подтверждение: %v", err)
	}
	if got := confirmed.EndsOn.Format("2006-01-02"); got != "2026-04-30" {
		t.Fatalf("подтверждённый конец %s, ожидался 2026-04-30", got)
	}
	if confirmed.EndsOnConfirmedAt == nil || confirmed.EndsOnConfirmedBy == nil {
		t.Error("отметка о подтверждении не проставлена")
	}

	// Цепочка поехала: второй период начинается 1 мая.
	p2, err = svc.Period(ctx, pid, 2, now)
	if err != nil {
		t.Fatalf("период 2 после сдвига: %v", err)
	}
	if got := p2.StartsOn.Format("2006-01-02"); got != "2026-05-01" {
		t.Errorf("начало периода 2 после сдвига %s, ожидалось 2026-05-01", got)
	}
	if got := p2.EndsOn.Format("2006-01-02"); got != "2026-05-31" {
		t.Errorf("конец периода 2 после сдвига %s, ожидался 2026-05-31", got)
	}

	// Подытоженный период больше не подтверждают.
	locked, err := svc.LockPeriod(ctx, confirmed, nil, now, now)
	if err != nil {
		t.Fatalf("подытог: %v", err)
	}
	if _, err := repo.ConfirmPeriodEnd(ctx, locked.ID,
		time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC), creators[0], now); err == nil {
		t.Error("подытоженному периоду подвинули границу")
	}
}
