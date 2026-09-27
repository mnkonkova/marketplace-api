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
func TestSteppedPeriodsDoNotCarry(t *testing.T) {
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

	// Гарантия и перенос выключены (сентябрь 2026): каждый период стоит
	// столько, сколько набрал, и ничего не передаёт дальше. Долг не
	// записывается вовсе — ни на входе, ни на выходе.
	want := []struct {
		seq       int
		total     int64
		debtIn    int64
		debtOut   int64
		aboutWhat string
	}{
		{1, 65_000 * rubles, 0, 0, "первый период — фикс за запуск"},
		{2, 72_000 * rubles, 0, 0, "200 000 просмотров — две ступени по факту"},
		{3, 84_000 * rubles, 0, 0, "400 000 просмотров — четыре ступени, долга нет"},
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

// Раскладка периода между ДВУМЯ креаторами — вживую, через базу.
//
// Цена периода — величина проекта: ступень берётся общим объёмом, фикс
// платится за каждый вышедший ролик. А строка человека отвечает на
// вопрос «сколько из этого моё», и делится каждая часть по своему
// основанию: фикс — по роликам, виральный хвост — по просмотрам СВЕРХ
// порога. Одним креатором эти правила неразличимы: любая доля равна
// единице, и ошибка проявляется только когда людей двое.
func TestPeriodSplitBetweenTwoCreators(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	terms := steppedProjectTerms(pid)
	fee := int64(1_000 * rubles)
	creatorFee := int64(500 * rubles)
	terms.FeePerVideo = &fee
	terms.CreatorFeePerVideo = &creatorFee
	if _, err := svc.SaveTerms(ctx, terms, creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}

	// Первый: два ролика, оба скромные — сверх порога ноль.
	seedPublicationViews(t, pid, creators[0], nextDue(), "two-a1", 200_000, true)
	seedPublicationViews(t, pid, creators[0], nextDue(), "two-a2", 300_000, true)
	// Второй: один ролик, и он залетел — весь виральный хвост его.
	seedPublicationViews(t, pid, creators[1], nextDue(), "two-b1", 2_500_000, true)

	rows, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	byCreator := map[uuid.UUID]billing.Accrual{}
	for _, a := range rows {
		byCreator[a.CreatorUserID] = a
	}
	first, second := byCreator[creators[0]], byCreator[creators[1]]

	// Фикс — по роликам: два из трёх и один из трёх. По просмотрам это
	// было бы 17% и 83%, то есть работа оплачена по удаче.
	if first.PayoutSalary != 2*creatorFee || second.PayoutSalary != creatorFee {
		t.Errorf("фикс разложен %d/%d, ожидалось %d/%d (два ролика и один)",
			first.PayoutSalary, second.PayoutSalary, 2*creatorFee, creatorFee)
	}
	// Хвост — по просмотрам СВЕРХ порога, а сверх него набрал только
	// второй. Ровный исполнитель доли чужой виральности не получает.
	if first.ViewsBonus != 0 {
		t.Errorf("хвост первому %d, а порога он не переходил", first.ViewsBonus)
	}
	if second.ViewsBonus <= 0 {
		t.Errorf("хвост второму %d, а залетел ролик именно у него", second.ViewsBonus)
	}

	// И главное правило подачи: сумма строк равна ЦЕНЕ ПЕРИОДА. Цену
	// считаем теми же ступенями, что и расчёт, но по известным фактам:
	// три сданных ролика, просмотры до порога и сверх него — те, что
	// посеяны выше.
	ladder := terms.ClientLadder()
	price := func(base, over int64, videos int64) int64 {
		fee, _ := ladder.Fee(1, base, videos)
		return fee + ladder.Tail(over)
	}
	// Порог на ролик — миллион: 200 000 + 300 000 + 1 000 000 до него,
	// 1 500 000 сверх.
	want := price(1_500_000, 1_500_000, 3)
	if got := first.Total + second.Total; got != want {
		t.Errorf("сумма строк %d, цена периода %d", got, want)
	}

	// Утверждённая строка — замороженные деньги: пересчёт её не трогает.
	// А остальным делится ОСТАТОК цены периода, а не вся цена: иначе
	// после утверждения сумма строк разойдётся с ценой ровно на
	// утверждённое — и «оклады + бонус» в шапке перестанут сходиться со
	// столбцом «итого» под таблицей.
	if _, err := svc.ApproveAccrual(ctx, first.ID, creators[0]); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Просмотры выросли вдвое — цена периода стала другой, а
	// утверждённая строка прежней.
	if _, err := pool.Exec(ctx, `
UPDATE video_stat_daily SET views = views * 2
WHERE link_id IN (SELECT l.id FROM publication_links l
                  JOIN project_publications p ON p.id = l.publication_id
                  WHERE p.project_id = $1)`, pid); err != nil {
		t.Fatalf("bump views: %v", err)
	}
	after, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc 2: %v", err)
	}
	var sum int64
	for _, a := range after {
		if a.ID == first.ID && a.Total != first.Total {
			t.Errorf("утверждённую строку переписали: было %d, стало %d", first.Total, a.Total)
		}
		sum += a.Total
	}
	// 400 000 + 600 000 + 1 000 000 до порога, 4 000 000 сверх.
	wantAfter := price(2_000_000, 4_000_000, 3)
	if sum != wantAfter {
		t.Errorf("после утверждения сумма строк %d, цена периода %d", sum, wantAfter)
	}
}
