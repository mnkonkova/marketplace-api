package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/billing"
	"marketpclce/tests/integration"
)

// Лесенка произвольной длины: пороги объёма и цена периода на каждом.
//
// Отличается от прежней модели устройством, а не числами: там ступень
// была блоком одинакового размера (step_views) и её цена лежала в
// колонке; здесь ступеней сколько угодно, они неравной высоты и лежат
// строками (миграция 00054). Из-за этого у них НЕТ «размера ступени», и
// любое место, которое берёт его из старого одиночного поля, на такой
// версии условий разыменовывает nil.
//
// Ровно так и случилось: кабинет креатора падал паникой на каждом
// проекте, заведённом на ступенчатой версии. Прогоны этого не увидели —
// посеянный мир стоял на снимке старого тарифа, и ветку с лесенкой не
// проходил ни один тест.

func ladderProjectTerms(pid uuid.UUID) billing.Terms {
	i := func(v int64) *int64 { return &v }
	return billing.Terms{
		ProjectID: pid,
		// Виральный хвост считается и здесь: порог стоит на ролике, а
		// ступени — на объёме периода. Одно другого не заменяет.
		RatePer1000Views:     60 * rubles,
		BonusViewsThreshold:  1_000_000,
		RatePer1000ViewsOver: 6 * rubles,

		// Оклад плюс KPI: нижняя ступень с порогом 0 — это и есть голый
		// оклад, дальше цена периода растёт скачками.
		Steps: []billing.TermsStep{
			{FromViews: 0, ClientFee: 60_000 * rubles, CreatorFee: i(40_000 * rubles)},
			{FromViews: 300_000, ClientFee: 78_000 * rubles, CreatorFee: i(52_000 * rubles)},
			{FromViews: 1_000_000, ClientFee: 120_000 * rubles},
		},
		// Стороны KPI разные: заказчик платит 12 ₽ за подписчика, креатор
		// получает 8 ₽. Пустая креаторская ставка означала бы «как у
		// заказчика», и маржи на этом KPI не было бы вовсе.
		SubscriberRate:        i(12 * rubles),
		CreatorSubscriberRate: i(8 * rubles),
	}
}

// Кабинет креатора открывается на проекте со ступенями-строками.
//
// Тест «ничего не упало» выглядит слабым, но ловит он не пустяк: до
// правки здесь была паника в nil-разыменовании, а наружу она выходила
// пятисоткой с пустым телом — экран заработка у креатора просто не
// открывался.
func TestCreatorEarningsOnLadderTerms(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, ladderProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "ldr01", 120_000)
	now := first.AddDate(0, 0, 10)

	got, err := svc.CreatorEarnings(ctx, pid, creators[0], now)
	if err != nil {
		t.Fatalf("кабинет креатора: %v", err)
	}

	// Прогноз обязан быть и обязан считаться от ЛЕСЕНКИ, а не от
	// «размера ступени»: следующая точка — 300 000, набрано 120 000.
	f := got.NextStep
	if f == nil {
		t.Fatalf("прогноза нет, а следующая ступень есть: набрано 120 000, порог 300 000")
	}
	if f.ViewsToGo != 180_000 {
		t.Errorf("до ступени %d просмотров, ожидалось 180 000", f.ViewsToGo)
	}
	// Высота ИМЕННО ЭТОЙ ступени — от взятого порога (0) до следующего.
	if f.StepViews != 300_000 {
		t.Errorf("высота ступени %d, ожидалось 300 000", f.StepViews)
	}
	// Ступень поднимает оклад креатора с 40 000 до 52 000.
	if f.ForecastPayout != 12_000*rubles {
		t.Errorf("прибавка %d, ожидалось %d", f.ForecastPayout, 12_000*rubles)
	}
}

// Взята верхняя ступень — прогноза нет вовсе.
//
// Не ноль и не «осталось 0»: тариф дальше не растёт, и обещание «ещё
// немного — и прибавят» было бы неправдой. Пустой прогноз фронт умеет
// не рисовать, а выдуманное число рисует как настоящее.
func TestNoForecastAboveTopLadderStep(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, ladderProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC)
	// Выше верхнего порога, но ниже порога на ролик: весь объём идёт в
	// ступени, а не в виральный хвост.
	publishOn(t, pool, pid, creators[0], first, "ldr02", 999_000)
	publishOn(t, pool, pid, creators[0], first.AddDate(0, 0, 1), "ldr03", 999_000)
	now := first.AddDate(0, 0, 10)

	got, err := svc.CreatorEarnings(ctx, pid, creators[0], now)
	if err != nil {
		t.Fatalf("кабинет креатора: %v", err)
	}
	if got.NextStep != nil {
		t.Errorf("верхняя ступень взята, а прогноз есть: %+v", got.NextStep)
	}
}

// Ступени доезжают до счёта: цена периода — у последней взятой ступени.
//
// Не сумма пройденных: «оклад плюс KPI на 300 000» означает одну цену за
// период, и складывать ступени нельзя.
func TestLadderChargesLastTakenStep(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, ladderProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 7, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "ldr04", 0)
	second := publishOn(t, pool, pid, creators[0], first.AddDate(0, 1, 1), "ldr05", 0)

	// Второй период: 350 000 — взята ступень 300 000, но не миллион.
	resetDailyViews(t, pool, pid)
	setLinkViewsOn(t, pool, second, first.AddDate(0, 1, 3), 350_000)

	p, err := svc.Period(ctx, pid, 2, first.AddDate(0, 1, 20))
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	rows, err := svc.Recalculate(ctx, pid, p)
	if err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	client, creator := sumTotal(rows)
	if client != 78_000*rubles {
		t.Errorf("счёт заказчику %d, ожидалось %d", client, 78_000*rubles)
	}
	// Своя цена той же ступени у креатора: стороны считаются раздельно.
	if creator != 52_000*rubles {
		t.Errorf("выплата креатору %d, ожидалось %d", creator, 52_000*rubles)
	}
}

// KPI по подписчикам доезжает до счёта — и до КАЖДОЙ строки.
//
// Подписчики устроены иначе, чем ступени: ступень — величина периода и
// делится между людьми по вкладу, а подписчиков вписывают каждому свои.
// Сначала так и было сломано: число сохранялось, а в начисление не
// попадало ни одной копейкой — суммы не менялись вовсе.
func TestSubscriberKPIReachesAccrual(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, ladderProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 8, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "ldr06", 100_000)
	now := first.AddDate(0, 0, 10)

	p, err := svc.Period(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	// 2 500 подписчиков по 12 ₽ с заказчика — 30 000 ₽ сверх ступени.
	if _, err := svc.SaveSubscribers(ctx, pid, creators[0], p.StartsOn, 2_500, creators[0]); err != nil {
		t.Fatalf("подписчики: %v", err)
	}

	rows, err := svc.Recalculate(ctx, pid, p)
	if err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	var a billing.Accrual
	for _, r := range rows {
		if r.CreatorUserID == creators[0] {
			a = r
		}
	}
	if a.CreatorUserID != creators[0] {
		t.Fatalf("строки того, кому вписали подписчиков, нет вовсе")
	}
	if a.Subscribers != 2_500 {
		t.Errorf("в строке %d подписчиков, ожидалось 2 500", a.Subscribers)
	}
	if a.SubscriberBonus != 30_000*rubles {
		t.Errorf("KPI заказчику %d, ожидалось %d", a.SubscriberBonus, 30_000*rubles)
	}
	// Своя ставка креатора: 8 ₽ вместо 12.
	if a.PayoutSubscriberBonus != 20_000*rubles {
		t.Errorf("KPI креатору %d, ожидалось %d", a.PayoutSubscriberBonus, 20_000*rubles)
	}
	// Итог строки включает KPI целиком: ступень между людьми делится по
	// вкладу, а подписчики — нет, они вписаны лично ему.
	if a.Total-a.Salary-a.ViewsBonus != 30_000*rubles {
		t.Errorf("KPI не попал в итог строки: %d", a.Total-a.Salary-a.ViewsBonus)
	}
	if a.PayoutTotal-a.PayoutSalary-a.PayoutViewsBonus != 20_000*rubles {
		t.Errorf("KPI не попал в выплату: %d", a.PayoutTotal-a.PayoutSalary-a.PayoutViewsBonus)
	}
}
