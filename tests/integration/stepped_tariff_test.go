package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/billing"
	"marketpclce/tests/integration"
)

// Ступенчатый тариф вживую: периоды, долг по гарантии и перенос остатка
// проходят через базу, а не только через арифметику.
//
// Саму лесенку и контрольные точки проверяет internal/billing:
// tariff_test.go — там между числом и проверкой нет ни SQL, ни HTTP.
// Здесь проверяется то, чего в чистых функциях не видно: что долг и
// перенос доезжают до следующего периода и что подытог их замораживает.

const rubles = 100 // копеек в рубле

func steppedProjectTerms(pid uuid.UUID) billing.Terms {
	i := func(v int64) *int64 { return &v }
	return billing.Terms{
		ProjectID: pid,
		// Виральный хвост: порог на ролик и пониженная ставка.
		RatePer1000Views:     60 * rubles,
		BonusViewsThreshold:  1_000_000,
		RatePer1000ViewsOver: 6 * rubles,

		StepViews:      i(100_000),
		FirstPeriodFee: i(65_000 * rubles),
		BaseFee:        i(60_000 * rubles),
		StepFee:        i(6_000 * rubles),
		StepTier2From:  i(2_000_000),
		StepFeeOver:    i(1_500 * rubles),
		StepCapViews:   i(3_000_000),
		GuaranteeViews: i(300_000),
	}
}

// setLinkViewsOn — просмотры КОНКРЕТНОГО ролика на конкретный день.
//
// Общего по проекту seedDailyViews здесь мало: он кладёт числа на первую
// ссылку проекта, а у ступенчатых тестов роликов несколько и лежат они в
// разных периодах.
//
// Кладём на одну площадку: порог стоит на ролике целиком, суммой по
// пяти, и одной достаточно.
func setLinkViewsOn(t *testing.T, pool *pgxpool.Pool, pubID uuid.UUID, day time.Time, views int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, collected_at)
SELECT l.id, $2::date, $3, 0, 0, $2::timestamptz
FROM publication_links l
WHERE l.publication_id = $1
ORDER BY l.platform
LIMIT 1
ON CONFLICT (link_id, stat_date) DO UPDATE SET views = EXCLUDED.views`,
		pubID, day, views); err != nil {
		t.Fatalf("просмотры ролика: %v", err)
	}
}

func setLinkViews(t *testing.T, pool *pgxpool.Pool, pubID uuid.UUID, views int64) {
	t.Helper()
	setLinkViewsOn(t, pool, pubID, time.Now().UTC(), views)
}

func periodDebt(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, seq int) (in, out int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT client_debt_in, client_debt_out FROM project_periods
WHERE project_id = $1 AND seq = $2`, pid, seq).Scan(&in, &out); err != nil {
		t.Fatalf("долг периода %d: %v", seq, err)
	}
	return in, out
}

func sumTotal(items []billing.Accrual) (client, creator int64) {
	for _, a := range items {
		client += a.Total
		creator += a.PayoutTotal
	}
	return client, creator
}

// Долг по гарантии живёт в периоде и гасится следующим.
//
// Первый период — фикс независимо от просмотров; второй недобирает
// гарантию и оплачивается как гарантия, оставляя долг В ПРОСМОТРАХ;
// третий этот долг гасит, и клиенту выставляется меньше ступеней, чем
// он набрал.
func TestSteppedGuaranteeDebtRollsToNextPeriod(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, steppedProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC)
	// Период 1: запуск, оплачивается фиксом. Просмотров нарочно мало —
	// на сумму они влиять не должны.
	publishOn(t, pool, pid, creators[0], first, "stp01", 140_000)
	// Период 2: недобор гарантии — 200 000 вместо 300 000.
	publishOn(t, pool, pid, creators[0], first.AddDate(0, 1, 2), "stp02", 200_000)
	// Период 3: 400 000, из которых 100 000 уйдут в погашение долга.
	publishOn(t, pool, pid, creators[0], first.AddDate(0, 2, 2), "stp03", 400_000)

	now := first.AddDate(0, 3, 0)

	want := []struct {
		seq       int
		total     int64
		debtIn    int64
		debtOut   int64
		aboutWhat string
	}{
		{1, 65_000 * rubles, 0, 0, "первый период — фикс за запуск"},
		{2, 78_000 * rubles, 0, 100_000, "недобор гарантии оплачен как гарантия"},
		{3, 78_000 * rubles, 100_000, 0, "долг погашен просмотрами следующего периода"},
	}
	for _, c := range want {
		p, err := svc.Period(ctx, pid, c.seq, now)
		if err != nil {
			t.Fatalf("период %d: %v", c.seq, err)
		}
		rows, err := svc.Recalculate(ctx, pid, p)
		if err != nil {
			t.Fatalf("пересчёт периода %d: %v", c.seq, err)
		}
		client, _ := sumTotal(rows)
		if client != c.total {
			t.Errorf("период %d (%s): счёт %d, ожидалось %d", c.seq, c.aboutWhat, client, c.total)
		}
		in, out := periodDebt(t, pool, pid, c.seq)
		if in != c.debtIn || out != c.debtOut {
			t.Errorf("период %d (%s): долг вход/выход %d/%d, ожидалось %d/%d",
				c.seq, c.aboutWhat, in, out, c.debtIn, c.debtOut)
		}
	}
}

// Подытоженный период не меняется, когда правят условия.
//
// Это главное свойство подытога: под ним уже стоит счёт, и правка прайса
// не должна переписывать историю. Перенос остатка и долг замораживаются
// вместе со срезом — иначе пересчёт поехал бы цепочкой по оплаченным
// периодам.
func TestLockedSteppedPeriodIgnoresNewTerms(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, steppedProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 4, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "stl01", 0)
	second := publishOn(t, pool, pid, creators[0], first.AddDate(0, 1, 1), "stl02", 0)

	// Второй период: 500 000 просмотров на отсечку — это 90 000 ₽.
	cutoff := first.AddDate(0, 2, 0).AddDate(0, 0, 14)
	resetDailyViews(t, pool, pid)
	setLinkViewsOn(t, pool, second, cutoff.AddDate(0, 0, -2), 500_000)

	p2, err := svc.Period(ctx, pid, 2, cutoff)
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	locked, err := svc.LockPeriod(ctx, p2, nil, cutoff, cutoff)
	if err != nil {
		t.Fatalf("подытог: %v", err)
	}
	if !locked.IsLocked() {
		t.Fatalf("период не подытожился: %+v", locked)
	}
	before, err := svc.ProjectBilling(ctx, pid, 2, cutoff)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	if before.Totals.Total != 90_000*rubles {
		t.Fatalf("подытог: %d, ожидалось %d", before.Totals.Total, 90_000*rubles)
	}

	// Выпускаем другой тариф: ступень вдвое дороже.
	newTerms := steppedProjectTerms(pid)
	dearer := int64(12_000 * rubles)
	newTerms.StepFee = &dearer
	if _, err := svc.SaveTerms(ctx, newTerms, creators[0]); err != nil {
		t.Fatalf("новые условия: %v", err)
	}

	after, err := svc.ProjectBilling(ctx, pid, 2, cutoff.AddDate(0, 0, 5))
	if err != nil {
		t.Fatalf("биллинг после правки: %v", err)
	}
	if after.Totals.Total != before.Totals.Total {
		t.Errorf("подытоженный период пересчитался по новым условиям: %d → %d",
			before.Totals.Total, after.Totals.Total)
	}
}

// Прогноз «до следующей ступени» обязан совпадать с настоящим
// пересчётом: он и считается тем же кодом. Разъедутся — креатору
// обещано одно, а начислено другое.
func TestSteppedForecastMatchesRealRecalculation(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, steppedProjectTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "stf01", 0)
	pub := publishOn(t, pool, pid, creators[0], first.AddDate(0, 1, 1), "stf02", 0)
	setLinkViews(t, pool, pub, 450_000)

	now := first.AddDate(0, 1, 20)
	p2, err := svc.Period(ctx, pid, 2, now)
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	before, err := svc.Recalculate(ctx, pid, p2)
	if err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	_, payoutBefore := sumTotal(before)

	earn, err := svc.CreatorEarnings(ctx, pid, creators[0], now)
	if err != nil {
		t.Fatalf("кабинет креатора: %v", err)
	}
	if earn.NextStep == nil {
		t.Fatal("прогноза до ступени нет")
	}
	if earn.NextStep.ViewsToGo != 50_000 {
		t.Fatalf("до ступени %d просмотров, ожидалось 50 000", earn.NextStep.ViewsToGo)
	}
	if earn.NextStep.ForecastPayout <= 0 {
		t.Fatal("прогноз нулевой — проверять нечего")
	}

	// Ролик добирает ровно столько, сколько обещал прогноз.
	setLinkViews(t, pool, pub, 450_000+earn.NextStep.ViewsToGo)
	after, err := svc.Recalculate(ctx, pid, p2)
	if err != nil {
		t.Fatalf("пересчёт после прироста: %v", err)
	}
	_, payoutAfter := sumTotal(after)

	if got := payoutAfter - payoutBefore; got != earn.NextStep.ForecastPayout {
		t.Errorf("прогноз обещал %d, начислено %d", earn.NextStep.ForecastPayout, got)
	}
}
