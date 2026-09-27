package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/billing"
	"marketpclce/internal/orders"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Деньги. Провайдера нет: клиент платит мимо системы, менеджер
// подтверждает кнопкой. Автоматически считается только бонус по
// просмотрам — их мы и так собираем каждый день.

// Тариф из референса: оклад 60 000 ₽/мес, 90 ₽ за 1000 просмотров до
// миллиона на ролик и 9 ₽ за 1000 свыше. Всё в копейках.
func demoTerms(projectID uuid.UUID) billing.Terms {
	return billing.Terms{
		ProjectID:            projectID,
		SalaryPerMonth:       6_000_000,
		RatePer1000Views:     9_000,
		BonusViewsThreshold:  1_000_000,
		RatePer1000ViewsOver: 900,
	}
}

// seedPublicationViews — выкладка с одной ссылкой и заданными просмотрами.
func seedPublicationViews(t *testing.T, projectID, creator uuid.UUID, due time.Time, url string, views int64, deliver bool) uuid.UUID {
	t.Helper()
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creator},
		Dates:          []time.Time{due},
		CreatedBy:      creator,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID
	if !deliver {
		return pubID
	}

	// Пять площадок — выкладка закрывается как done.
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creator,
		URLs: []string{
			"https://www.tiktok.com/@a/video/" + url,
			"https://www.instagram.com/reel/" + url + "/",
			"https://www.youtube.com/shorts/" + url,
			"https://vk.com/clip-1_" + url,
			"https://likee.video/@a/video/" + url,
		},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	// Просмотры кладём на одну площадку: порог считается по ролику
	// целиком, суммой по пяти, — этого достаточно, чтобы это проверить.
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM publication_links WHERE publication_id = $1 ORDER BY platform LIMIT 1`,
		pubID).Scan(&linkID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, collected_at)
VALUES ($1, CURRENT_DATE, $2, 0, 0, now())
ON CONFLICT (link_id, stat_date) DO UPDATE SET views = EXCLUDED.views`,
		linkID, views); err != nil {
		t.Fatalf("stats: %v", err)
	}
	return pubID
}

// seedPartialPublication — ролик вышел, но не на всех площадках.
//
// В период он попадает (ссылка есть, значит вышел), а сданным не
// считается: статус partial. Это и есть современная «недосдача» —
// «не вышел вовсе» в период не попадает, у него нет даты публикации.
func seedPartialPublication(t *testing.T, projectID, creator uuid.UUID, due time.Time, url string) uuid.UUID {
	t.Helper()
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creator},
		Dates:          []time.Time{due},
		CreatedBy:      creator,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creator,
		URLs:          []string{"https://www.tiktok.com/@a/video/" + url},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	return pubID
}

// ---- ТЕСТ: как считается начисление ----

// Имя креатора видно и ДО пересчёта.
//
// Менеджер открывает «Начисления» и первым видит предварительный расчёт —
// строки, посчитанные на лету, которых в базе ещё нет. Сохранённые
// начисления подтягивают имя прямо в запросе, а расчёт приходит из
// арифметики: в ней людей нет, только числа. На экране это выглядело как
// потеря данных — строка есть, деньги есть, напротив них «Без имени».
//
// Сторожим обе половины: имя должно стоять и в предварительном расчёте, и
// в сохранённом, и это должно быть ОДНО И ТО ЖЕ имя. Разойдись они — и
// строка «переименуется» от нажатия кнопки «Пересчитать».
func TestPreviewAccrualHasCreatorName(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "nm01", 500_000, true)

	// Предварительный расчёт: пересчёта ещё не было, строк в базе нет.
	now := time.Now().UTC()
	before, err := svc.ProjectBilling(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("сводка до пересчёта: %v", err)
	}
	preview := findAccrual(t, before.Accruals, creators[0])
	if !preview.IsPreview {
		t.Fatal("строка должна быть предварительной: пересчёта не было")
	}
	if preview.CreatorName == "" {
		t.Error("в предварительном расчёте нет имени креатора — на экране это «Без имени» напротив настоящих сумм")
	}

	// И после пересчёта имя то же самое.
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}
	after, err := svc.ProjectBilling(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("сводка после пересчёта: %v", err)
	}
	saved := findAccrual(t, after.Accruals, creators[0])
	if saved.CreatorName != preview.CreatorName {
		t.Errorf("имя разъехалось: до пересчёта %q, после %q — строка «переименовалась» от нажатия кнопки",
			preview.CreatorName, saved.CreatorName)
	}
}

// findAccrual — строка нужного креатора или падение с внятным текстом.
func findAccrual(t *testing.T, items []billing.Accrual, creator uuid.UUID) billing.Accrual {
	t.Helper()
	for _, a := range items {
		if a.CreatorUserID == creator {
			return a
		}
	}
	t.Fatalf("начисления креатора %s нет среди %d строк", creator, len(items))
	return billing.Accrual{}
}

// Креатор видит идущий период, а не «посчитаем потом».
//
// Сохранённая строка начисления появляется только после пересчёта: его
// делает менеджер кнопкой либо воркер при подытоге, то есть через две
// недели после конца периода. Пока период идёт, строки нет — и креатор
// видел пустоту при том, что на том же экране стоят его просмотры и
// взятые ступени. Просмотры измерены, тариф известен, ступени
// посчитаны, а деньги показать отказывались.
//
// Сторожим три вещи сразу, потому что их легко разменять:
// расчёт идущего периода приходит; он совпадает с тем, что даёт
// настоящий пересчёт (иначе экран обещал бы одно, а платили бы другое);
// и сохранённая строка расчётом на лету НЕ подменяется — по ней уже
// могли утвердить и выплатить.
func TestCreatorSeesCurrentPeriodBeforeRecalc(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "cur1", 500_000, true)

	now := time.Now().UTC()
	before, err := svc.CreatorEarnings(ctx, pid, creators[0], now)
	if err != nil {
		t.Fatalf("заработок до пересчёта: %v", err)
	}
	if before.Period == nil {
		t.Fatal("периода нет, хотя ролик вышел")
	}
	cur := creatorRowFor(t, before, before.Period.StartsOn)
	if cur.Total <= 0 {
		t.Fatalf("за идущий период показано %d — креатор видит пустоту там, где всё посчитано", cur.Total)
	}

	// То же число, что даст настоящий пересчёт: расчёт для показа и
	// расчёт для выплаты — один и тот же код, а не две формулы.
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}
	after, err := svc.CreatorEarnings(ctx, pid, creators[0], now)
	if err != nil {
		t.Fatalf("заработок после пересчёта: %v", err)
	}
	saved := creatorRowFor(t, after, before.Period.StartsOn)
	if saved.Total != cur.Total {
		t.Errorf("до пересчёта показали %d, после стало %d — экран обещал одно, а посчитали другое",
			cur.Total, saved.Total)
	}
	if len(after.Accruals) != len(before.Accruals) {
		t.Errorf("строк было %d, стало %d — расчёт на лету задвоился с сохранённым",
			len(before.Accruals), len(after.Accruals))
	}
}

// creatorRowFor — начисление за период, начавшийся в этот день.
func creatorRowFor(t *testing.T, e billing.CreatorEarnings, start time.Time) billing.CreatorAccrual {
	t.Helper()
	for _, a := range e.Accruals {
		if a.PeriodStart.Year() == start.Year() && a.PeriodStart.Month() == start.Month() &&
			a.PeriodStart.Day() == start.Day() {
			return a
		}
	}
	t.Fatalf("начисления за период с %s нет среди %d строк", start.Format("2006-01-02"), len(e.Accruals))
	return billing.CreatorAccrual{}
}

func TestAccrualArithmetic(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}

	// Первый креатор: три вышедших ролика, полностью сданы два. Один
	// перешагнул порог (2 000 000 просмотров — миллион по полной ставке
	// и миллион по пониженной), второй нет (500 000, вся по полной),
	// третий вышел, но не на всех площадках.
	//
	// Третий именно «вышел неполностью», а не «не вышел»: в период
	// попадают ролики по факту публикации, и не вышедший не попадает в
	// него вовсе — вычитать за него нечего и не из чего.
	seedPublicationViews(t, pid, creators[0], pubDay(0), "aaa1", 2_000_000, true)
	seedPublicationViews(t, pid, creators[0], pubDay(0).AddDate(0, 0, 1), "aaa2", 500_000, true)
	seedPartialPublication(t, pid, creators[0], pubDay(0).AddDate(0, 0, 2), "aaa3")

	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}

	var got *billing.Accrual
	for i := range items {
		if items[i].CreatorUserID == creators[0] {
			got = &items[i]
		}
	}
	if got == nil {
		t.Fatalf("начисление не посчиталось: %+v", items)
	}

	if got.VideosPlanned != 3 || got.VideosDelivered != 2 {
		t.Errorf("выкладки: planned=%d delivered=%d", got.VideosPlanned, got.VideosDelivered)
	}
	// Недосданное не оплачивается: треть оклада вычитается.
	if want := int64(6_000_000 / 3); got.Deduction != want {
		t.Errorf("вычет: want %d, got %d", want, got.Deduction)
	}
	// Ступени считаются ПО КАЖДОМУ РОЛИКУ и складываются:
	//   ролик на 2 000 000 → 1 000 000 по 90 ₽ + 1 000 000 по 9 ₽
	//   ролик на   500 000 →   500 000 по 90 ₽
	// Итого до порога 1 500 000, сверх — 1 000 000.
	if got.ViewsBase != 1_500_000 || got.ViewsOver != 1_000_000 {
		t.Errorf("ступени: base=%d over=%d", got.ViewsBase, got.ViewsOver)
	}
	want := int64(1_500_000/1000*9_000 + 1_000_000/1000*900)
	if got.ViewsBonus != want {
		t.Errorf("бонус: want %d, got %d", want, got.ViewsBonus)
	}
	if got.ViewsTotal != 2_500_000 {
		t.Errorf("всего просмотров: %d", got.ViewsTotal)
	}
	// Бонус за переходы выключен по умолчанию — он в статусе BETA.
	if got.ClickBonus != 0 {
		t.Errorf("бонус за переходы посчитался при выключенной ставке: %d", got.ClickBonus)
	}
	if wantTotal := got.Salary - got.Deduction + got.ViewsBonus; got.Total != wantTotal {
		t.Errorf("итог: want %d, got %d", wantTotal, got.Total)
	}

	// Второму креатору выкладок не ставили — месяц работой не был,
	// оклада за него нет.
	for _, a := range items {
		if a.CreatorUserID == creators[1] && a.Salary != 0 {
			t.Errorf("оклад без единой выкладки: %+v", a)
		}
	}
}

// Порог стоит НА РОЛИКЕ, а не на месяце: два ролика по 600 000 — это
// ноль сверхпорогового объёма, хотя в сумме за месяц 1 200 000.
func TestThresholdIsPerVideoNotPerMonth(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "fff1", 600_000, true)
	seedPublicationViews(t, pid, creators[0], pubDay(0).AddDate(0, 0, 1), "fff2", 600_000, true)

	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	for _, a := range items {
		if a.CreatorUserID != creators[0] {
			continue
		}
		if a.ViewsOver != 0 {
			t.Errorf("сверхпороговый объём появился там, где ни один ролик порога не перешёл: %d", a.ViewsOver)
		}
		if a.ViewsBase != 1_200_000 {
			t.Errorf("по полной ставке должно идти всё: %d", a.ViewsBase)
		}
		if want := int64(1_200_000 / 1000 * 9_000); a.ViewsBonus != want {
			t.Errorf("бонус: want %d, got %d", want, a.ViewsBonus)
		}
	}
}

// ---- ТЕСТ: порядок кнопок ----

func TestAccrualButtonsOrder(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "bbb1", 1_500_000, true)

	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	var acc billing.Accrual
	for _, a := range items {
		if a.CreatorUserID == creators[0] {
			acc = a
		}
	}
	if acc.Status != billing.AccrualDraft {
		t.Fatalf("свежее начисление должно быть черновиком: %s", acc.Status)
	}

	// Выплатить черновик нельзя: между «посчитали» и «отправили деньги»
	// должен стоять тот, кто на цифру посмотрел.
	if _, err := svc.MarkAccrualPaid(ctx, acc.ID, creators[0]); !errors.Is(err, billing.ErrWrongAccrualStatus) {
		t.Errorf("выплата черновика: want ErrWrongAccrualStatus, got %v", err)
	}

	approved, err := svc.ApproveAccrual(ctx, acc.ID, creators[0])
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != billing.AccrualApproved || approved.ApprovedAt == nil {
		t.Errorf("после утверждения: %+v", approved)
	}
	// Второй раз утвердить нельзя.
	if _, err := svc.ApproveAccrual(ctx, acc.ID, creators[0]); !errors.Is(err, billing.ErrWrongAccrualStatus) {
		t.Errorf("повторное утверждение: %v", err)
	}

	paid, err := svc.MarkAccrualPaid(ctx, acc.ID, creators[0])
	if err != nil {
		t.Fatalf("paid: %v", err)
	}
	if paid.Status != billing.AccrualPaid || paid.PaidAt == nil {
		t.Errorf("после выплаты: %+v", paid)
	}
}

// Утверждённое начисление пересчёт не трогает: цифра, по которой уже
// перевели деньги, задним числом не меняется.
func TestApprovedAccrualSurvivesRecalc(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "ccc1", 1_200_000, true)

	items, _ := recalcCurrent(t, svc, pid)
	var acc billing.Accrual
	for _, a := range items {
		if a.CreatorUserID == creators[0] {
			acc = a
		}
	}
	if _, err := svc.ApproveAccrual(ctx, acc.ID, creators[0]); err != nil {
		t.Fatalf("approve: %v", err)
	}
	before := acc.Total

	// Просмотры выросли — но период уже утверждён.
	if _, err := pool.Exec(ctx, `
UPDATE video_stat_daily SET views = views * 10
WHERE link_id IN (SELECT l.id FROM publication_links l
                  JOIN project_publications p ON p.id = l.publication_id
                  WHERE p.project_id = $1)`, pid); err != nil {
		t.Fatalf("bump views: %v", err)
	}
	after, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc 2: %v", err)
	}
	for _, a := range after {
		if a.ID == acc.ID && a.Total != before {
			t.Errorf("утверждённое начисление переписали: было %d, стало %d", before, a.Total)
		}
	}
}

// ---- ТЕСТ: платежи ----

func TestPaymentConfirmIsOnceAndFinal(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	// Подтверждать нечего, пока платёж не заведён.
	if _, err := svc.ConfirmPayment(ctx, pid, billing.PaymentPrepayment, creators[0]); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("подтверждение несуществующего: want ErrNotFound, got %v", err)
	}

	if _, err := svc.SetPayment(ctx, pid, billing.PaymentPrepayment, 3_000_000, "половина вперёд", creators[0]); err != nil {
		t.Fatalf("set: %v", err)
	}
	p, err := svc.ConfirmPayment(ctx, pid, billing.PaymentPrepayment, creators[0])
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if p.Status != billing.PaymentConfirmed || p.ConfirmedBy == nil || p.ConfirmedAt == nil {
		t.Errorf("подтверждение именное и со временем: %+v", p)
	}

	// Второй раз — уже подтверждено, а не «ок».
	if _, err := svc.ConfirmPayment(ctx, pid, billing.PaymentPrepayment, creators[0]); !errors.Is(err, billing.ErrAlreadyConfirmed) {
		t.Errorf("повторное подтверждение: want ErrAlreadyConfirmed, got %v", err)
	}
	// И сумму задним числом не переписать.
	if _, err := svc.SetPayment(ctx, pid, billing.PaymentPrepayment, 1, "передумал", creators[0]); !errors.Is(err, billing.ErrAlreadyConfirmed) {
		t.Errorf("правка подтверждённой суммы: want ErrAlreadyConfirmed, got %v", err)
	}
}

// ---- ТЕСТ: кто что видит ----

func TestBillingVisibility(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "ddd1", 1_100_000, true)
	seedPublicationViews(t, pid, creators[1], pubDay(0), "ddd2", 1_100_000, true)
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}

	// Креатор видит только свою строку.
	mine, err := svc.CreatorEarnings(ctx, pid, creators[0], time.Now().UTC())
	if err != nil {
		t.Fatalf("earnings: %v", err)
	}
	if len(mine.Accruals) != 1 || mine.Accruals[0].CreatorUserID != creators[0] {
		t.Errorf("креатору видны чужие начисления: %+v", mine.Accruals)
	}
	if mine.Terms.SalaryPerMonth != 6_000_000 {
		t.Errorf("креатор не видит условий: %+v", mine.Terms)
	}

	// Заказчик видит состав месяца: он за эту команду платит, и строка
	// «60 000 + 5 850» — его счёт. А вот UTM-метки ему не отдаются:
	// это рабочий инструмент менеджера.
	client, err := svc.ClientBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("client billing: %v", err)
	}
	if len(client.Accruals) != 2 {
		t.Errorf("заказчику не показали команду месяца: %+v", client.Accruals)
	}
	if client.Totals.Total == 0 {
		t.Errorf("итог «к оплате» не посчитан: %+v", client.Totals)
	}
	// UTM-меток в клиентском ответе нет уже на уровне типа: поля в
	// ClientBillingView не существует, и проверять тут больше нечего.
	// Что именно уезжает по сети — см. TestClientBillingHidesOurMoney:
	// проверка по сырому JSON, а не по структуре Go.

	// Менеджер видит обе строки.
	all, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("project billing: %v", err)
	}
	if len(all.Accruals) != 2 {
		t.Errorf("менеджеру должно быть видно два начисления: %+v", all.Accruals)
	}
	for _, a := range all.Accruals {
		if a.CreatorName == "" {
			t.Errorf("начисление без имени креатора: %+v", a)
		}
	}
}

// ---- ТЕСТ: UTM ----

func TestUTMSetByManager(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	if _, err := svc.SaveUTM(ctx, pid, creators[0], "javascript:alert(1)", creators[0]); !errors.Is(err, billing.ErrInvalidInput) {
		t.Errorf("опасная ссылка прошла: %v", err)
	}
	l, err := svc.SaveUTM(ctx, pid, creators[0], "https://petflat.ru/?utm_source=nastya", creators[0])
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if l.Clicks != 0 {
		t.Errorf("переходы заполняются снаружи, а не при создании: %d", l.Clicks)
	}

	// Повторная запись меняет ссылку, а не заводит вторую.
	if _, err := svc.SaveUTM(ctx, pid, creators[0], "https://petflat.ru/?utm_source=nastya2", creators[0]); err != nil {
		t.Fatalf("update: %v", err)
	}
	links, err := svc.UTM(ctx, pid, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(links) != 1 || links[0].URL != "https://petflat.ru/?utm_source=nastya2" {
		t.Errorf("метка: %+v", links)
	}
}

// Ставка за переход выключена по умолчанию (BETA). Включённая — считается.
func TestClickBonusOnlyWhenEnabled(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	terms := demoTerms(pid)
	// 70 ₽ за первую тысячу переходов ЗА МЕСЯЦ, 7 ₽ за каждый следующий.
	rate, rateOver := int64(7_000), int64(700)
	terms.ClickBonusRate = &rate
	terms.ClickBonusThreshold = 1000
	terms.ClickBonusRateOver = &rateOver
	if _, err := svc.SaveTerms(ctx, terms, creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "eee1", 100, true)
	if _, err := svc.SaveUTM(ctx, pid, creators[0], "https://petflat.ru/?utm_source=x", creators[0]); err != nil {
		t.Fatalf("utm: %v", err)
	}
	// 1200 переходов: тысяча по полной ставке, двести по пониженной.
	if _, err := pool.Exec(ctx,
		`UPDATE creator_utm_links SET clicks = 1200 WHERE project_id = $1 AND creator_user_id = $2`,
		pid, creators[0]); err != nil {
		t.Fatalf("clicks: %v", err)
	}

	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	for _, a := range items {
		if a.CreatorUserID != creators[0] {
			continue
		}
		if a.Clicks != 1200 {
			t.Errorf("переходы не доехали: %d", a.Clicks)
		}
		if want := int64(1000*7_000 + 200*700); a.ClickBonus != want {
			t.Errorf("бонус за переходы: want %d, got %d", want, a.ClickBonus)
		}
	}
}

// Итог периода считается на сервере: и менеджер, и заказчик показывают
// одно и то же число, а не складывают строки каждый у себя.
func TestPeriodTotals(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "ggg1", 1_000_000, true)
	seedPublicationViews(t, pid, creators[1], pubDay(0), "ggg2", 1_000_000, true)
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}

	out, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing: %v", err)
	}
	var sum, views int64
	for _, a := range out.Accruals {
		sum += a.Total
		views += a.ViewsTotal
	}
	if out.Totals.Total != sum {
		t.Errorf("итог: want %d, got %d", sum, out.Totals.Total)
	}
	if out.Totals.Views != views {
		t.Errorf("просмотры: want %d, got %d", views, out.Totals.Views)
	}
	if out.Totals.Videos == 0 || out.Totals.VideosDelivered == 0 {
		t.Errorf("ролики в итоге не посчитаны: %+v", out.Totals)
	}
	if out.Totals.CostPer1000 == nil {
		t.Fatalf("не посчитана стоимость тысячи просмотров")
	}
	if want := sum * 1000 / views; *out.Totals.CostPer1000 != want {
		t.Errorf("стоимость 1000: want %d, got %d", want, *out.Totals.CostPer1000)
	}
}

// ---- ТЕСТ: смета заказа ----

// Оклады известны точно, бонус — прогноз по истории тех, кто в подборке.
func TestOrderEstimate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanupOrders := setupOrderWorld(t, pool)
	defer cleanupOrders()

	// Тариф кладём в действующую версию правил: смета берёт его оттуда,
	// а не из проекта — проекта на момент заказа ещё нет.
	if _, err := pool.Exec(ctx, `
UPDATE terms_versions SET salary_per_month = 6000000, rate_per_1000_views = 9000,
       bonus_views_threshold = 1000000, rate_per_1000_views_over = 900`); err != nil {
		t.Fatalf("terms: %v", err)
	}

	ordersSvc := orders.NewService(orders.NewRepo(pool))
	res, err := ordersSvc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:3],
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc := billing.NewService(billing.NewRepo(pool))

	// Истории нет ни у кого: бонус не ноль, а неизвестен.
	est, err := svc.EstimateOrder(ctx, res.Order.ID, clientID)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.HasForecast {
		t.Errorf("прогноз построен на пустой истории: %+v", est)
	}
	if est.WithoutHistory != 3 {
		t.Errorf("без истории должны быть все трое, посчитано %d", est.WithoutHistory)
	}
	if est.Salaries != 6_000_000 || est.Total != est.Salaries {
		t.Errorf("оклады известны точно и составляют весь итог: %+v", est)
	}

	// Даём одному из подборки историю: два ролика по 2 000 000.
	pid, projCreators, cleanupProject := setupCreatorsProject(t, pool)
	defer cleanupProject()
	_ = projCreators
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_creators (project_id, creator_user_id) VALUES ($1, $2)`,
		pid, creators[0]); err != nil {
		t.Fatalf("add to project: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "hhh1", 2_000_000, true)
	seedPublicationViews(t, pid, creators[0], pubDay(0).AddDate(0, 0, 1), "hhh2", 2_000_000, true)

	est, err = svc.EstimateOrder(ctx, res.Order.ID, clientID)
	if err != nil {
		t.Fatalf("estimate 2: %v", err)
	}
	if !est.HasForecast {
		t.Fatalf("прогноз не построился при живой истории: %+v", est)
	}
	if est.WithoutHistory != 2 {
		t.Errorf("без истории остались двое, посчитано %d", est.WithoutHistory)
	}
	// Среднее считается только по тем, у кого история есть: 2 000 000, а
	// не 666 666 — иначе новички занижали бы смету самим фактом присутствия.
	if est.AvgViewsPerVideo != 2_000_000 {
		t.Errorf("среднее: %d", est.AvgViewsPerVideo)
	}
	if est.ViewsForecast != 2_000_000*30 {
		t.Errorf("прогноз просмотров: %d", est.ViewsForecast)
	}
	// Ступени считаются НА РОЛИК: миллион по 90 ₽ и миллион по 9 ₽,
	// тридцать раз.
	wantBonus := int64(30) * (1_000_000/1000*9_000 + 1_000_000/1000*900)
	if est.BonusForecast != wantBonus {
		t.Errorf("прогноз бонуса: want %d, got %d", wantBonus, est.BonusForecast)
	}
	if est.Total != est.Salaries+est.BonusForecast {
		t.Errorf("итог: %+v", est)
	}

	// Чужую смету не посмотреть.
	if _, err := svc.EstimateOrder(ctx, res.Order.ID, uuid.New()); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("чужой заказ: want ErrNotFound, got %v", err)
	}
}

// Приоритет из подборки доезжает до начисления: «команда собрана по
// вашему приоритету» — это про него. У проекта, заведённого руками,
// приоритета нет и быть не должно.
func TestAccrualCarriesOrderPriority(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "iii1", 100_000, true)
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}

	items, err := svc.Accruals(ctx, pid, nil, &creators[0])
	if err != nil {
		t.Fatalf("accruals: %v", err)
	}
	if len(items) != 1 || items[0].Priority != 0 {
		t.Errorf("у проекта без заказа приоритета быть не должно: %+v", items)
	}

	// Привязываем проект к заказу, где этот креатор стоял вторым.
	// Версия правил нужна заказу как обязательная ссылка; в чистой базе
	// её может не быть — заводим свою.
	var termsID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO terms_versions (version, body)
VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions), 'для теста')
RETURNING id`).Scan(&termsID); err != nil {
		t.Fatalf("terms version: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, termsID)
	var clientID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, pid).Scan(&clientID); err != nil {
		t.Fatalf("client: %v", err)
	}
	var orderID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO creator_orders (client_user_id, start_month, needed, videos_count, terms_version_id, project_id)
VALUES ($1, date_trunc('month', CURRENT_DATE)::date, 1, 30, $2, $3) RETURNING id`,
		clientID, termsID, pid).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM creator_orders WHERE id = $1`, orderID)
	if _, err := pool.Exec(ctx, `
INSERT INTO order_candidates (order_id, creator_user_id, priority) VALUES ($1, $2, 2)`,
		orderID, creators[0]); err != nil {
		t.Fatalf("candidate: %v", err)
	}

	items, err = svc.Accruals(ctx, pid, nil, &creators[0])
	if err != nil {
		t.Fatalf("accruals 2: %v", err)
	}
	if len(items) != 1 || items[0].Priority != 2 {
		t.Errorf("приоритет не доехал: %+v", items)
	}
}

// ---- ТЕСТ: выплата креатору отличается от счёта клиенту ----

// Две стороны тарифа. Пока креаторская не задана, суммы совпадают и маржи
// нет — это поведение по умолчанию, и оно не должно измениться от самого
// факта появления второй стороны.
func TestPayoutEqualsChargeWithoutMargin(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "jjj1", 1_500_000, true)
	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	for _, a := range items {
		if a.CreatorUserID != creators[0] {
			continue
		}
		if a.PayoutTotal != a.Total {
			t.Errorf("без своей ставки выплата обязана равняться счёту: %d против %d",
				a.PayoutTotal, a.Total)
		}
	}
	out, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing: %v", err)
	}
	if out.Totals.Margin != 0 {
		t.Errorf("маржи без второй стороны тарифа быть не может: %d", out.Totals.Margin)
	}
}

// А с заданной креаторской ставкой суммы расходятся, и разница — маржа.
func TestPayoutDiffersWhenCreatorRatesSet(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	terms := demoTerms(pid)
	// Клиент платит 60 000 ₽ и 90 ₽/1000; креатор получает 45 000 ₽ и
	// 70 ₽/1000 до порога, 7 ₽ свыше.
	creatorSalary, creatorRate, creatorOver := int64(4_500_000), int64(7_000), int64(700)
	terms.CreatorSalaryPerMonth = &creatorSalary
	terms.CreatorRatePer1000Views = &creatorRate
	terms.CreatorRatePer1000ViewsOver = &creatorOver
	if _, err := svc.SaveTerms(ctx, terms, creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}

	// Один ролик на 2 000 000: миллион по полной ставке, миллион по
	// пониженной — на каждой стороне по своим числам.
	seedPublicationViews(t, pid, creators[0], pubDay(0), "kkk1", 2_000_000, true)
	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}

	var acc billing.Accrual
	for _, a := range items {
		if a.CreatorUserID == creators[0] {
			acc = a
		}
	}
	wantCharge := int64(6_000_000) + 1_000_000/1000*9_000 + 1_000_000/1000*900
	wantPayout := int64(4_500_000) + 1_000_000/1000*7_000 + 1_000_000/1000*700
	if acc.Total != wantCharge {
		t.Errorf("счёт клиенту: want %d, got %d", wantCharge, acc.Total)
	}
	if acc.PayoutTotal != wantPayout {
		t.Errorf("выплата креатору: want %d, got %d", wantPayout, acc.PayoutTotal)
	}
	if acc.PayoutSalary != 4_500_000 {
		t.Errorf("оклад креатора: %d", acc.PayoutSalary)
	}

	out, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing: %v", err)
	}
	if out.Totals.Margin != wantCharge-wantPayout {
		t.Errorf("маржа: want %d, got %d", wantCharge-wantPayout, out.Totals.Margin)
	}
	if out.Totals.Payouts != wantPayout {
		t.Errorf("сумма выплат: %d", out.Totals.Payouts)
	}
}

// Креатор видит СВОИ деньги, а не цену клиента.
func TestCreatorSeesOwnPayoutNotClientPrice(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	terms := demoTerms(pid)
	creatorSalary := int64(4_500_000)
	terms.CreatorSalaryPerMonth = &creatorSalary
	if _, err := svc.SaveTerms(ctx, terms, creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "lll1", 100_000, true)
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}

	earn, err := svc.CreatorEarnings(ctx, pid, creators[0], time.Now().UTC())
	if err != nil {
		t.Fatalf("earnings: %v", err)
	}
	if earn.Terms.SalaryPerMonth != creatorSalary {
		t.Errorf("креатору показан оклад клиента: %d", earn.Terms.SalaryPerMonth)
	}
	// Второй стороны тарифа в ответе нет уже на уровне типа: у SideTerms
	// креаторских полей не существует. Что уезжает по сети — проверяет
	// TestCreatorEarningsHideClientPrice по сырому JSON.
	if len(earn.Accruals) != 1 {
		t.Fatalf("начислений: %d", len(earn.Accruals))
	}
	a := earn.Accruals[0]
	if a.Salary != creatorSalary {
		t.Errorf("в строке креатора должны стоять его числа: %+v", a)
	}
	// Цена клиента за тот же месяц заведомо другая — убеждаемся, что в
	// итог креатора она не просочилась.
	all, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("project billing: %v", err)
	}
	for _, m := range all.Accruals {
		if m.CreatorUserID != creators[0] {
			continue
		}
		if a.Total != m.PayoutTotal {
			t.Errorf("креатору показан не его итог: %d вместо %d", a.Total, m.PayoutTotal)
		}
		if a.Total == m.Total {
			t.Errorf("итог креатора совпал со счётом клиента (%d) — числа не разошлись, "+
				"тест ничего не проверяет", m.Total)
		}
	}
}

// Креатор без единой выкладки не получает ничего.
//
// Ловушка была в SQL: LEFT JOIN даёт по нему строку с NULL, а
// LEAST(NULL, порог) в PostgreSQL возвращает порог, а не NULL. Человек,
// которому не поставили ни одного ролика, получал бонус за миллион
// просмотров — и в счёт клиенту это тоже попадало.
func TestCreatorWithoutPublicationsGetsNothing(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	// Работает только первый; второй в составе, но выкладок ему не ставили.
	seedPublicationViews(t, pid, creators[0], pubDay(0), "mmm1", 2_000_000, true)

	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	var idle billing.Accrual
	for _, a := range items {
		if a.CreatorUserID == creators[1] {
			idle = a
		}
	}
	if idle.ViewsBase != 0 || idle.ViewsOver != 0 || idle.ViewsTotal != 0 {
		t.Errorf("просмотры у неработавшего: base=%d over=%d total=%d",
			idle.ViewsBase, idle.ViewsOver, idle.ViewsTotal)
	}
	if idle.ViewsBonus != 0 || idle.Total != 0 || idle.PayoutTotal != 0 {
		t.Errorf("деньги у неработавшего: бонус=%d счёт=%d выплата=%d",
			idle.ViewsBonus, idle.Total, idle.PayoutTotal)
	}
}

// ---- прайс площадки ----

// Новая версия прайса не переписывает старую: под старой стоит согласие
// клиентов, а проекты сняли с неё числа снимком. Проверяем, что версия
// выпускается следующим номером, становится действующей, а прежняя
// остаётся на месте нетронутой.
func TestPublishTermsVersionKeepsPrevious(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	before, err := svc.ListTermsVersions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	creatorSalary, creatorRate, creatorOver := int64(4_500_000), int64(7_000), int64(700)
	v, err := svc.PublishTermsVersion(ctx, billing.TermsVersion{
		Terms: billing.Terms{
			SalaryPerMonth:              6_000_000,
			VideosFirstMonth:            30,
			VideosNextMonths:            60,
			RatePer1000Views:            9_000,
			BonusViewsThreshold:         1_000_000,
			RatePer1000ViewsOver:        900,
			CreatorSalaryPerMonth:       &creatorSalary,
			CreatorRatePer1000Views:     &creatorRate,
			CreatorRatePer1000ViewsOver: &creatorOver,
		},
		Body: "Условия работы, версия из теста.",
	}, uuid.Nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	t.Cleanup(func() {
		if v.TermsVersionID != nil {
			_, _ = pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, *v.TermsVersionID)
		}
	})

	after, err := svc.ListTermsVersions(ctx)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("версий стало %d, ожидали %d", len(after), len(before)+1)
	}
	if !after[0].IsCurrent {
		t.Error("свежая версия обязана быть действующей")
	}
	if len(before) > 0 && after[1].Version != before[0].Version {
		t.Errorf("прежняя версия подменилась: было %d, стало %d",
			before[0].Version, after[1].Version)
	}
	for _, old := range after[1:] {
		if old.IsCurrent {
			t.Errorf("действующей осталась и версия %d", old.Version)
		}
	}
}

// Доля креатора выше цены клиента — не тариф, а убыток на каждом ролике,
// и почти всегда опечатка. Такую версию выпускать нельзя.
func TestPublishTermsRejectsCreatorShareAboveClientPrice(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	tooMuch := int64(90_000)
	_, err := svc.PublishTermsVersion(ctx, billing.TermsVersion{
		Terms: billing.Terms{
			SalaryPerMonth:          6_000_000,
			RatePer1000Views:        9_000,
			BonusViewsThreshold:     1_000_000,
			RatePer1000ViewsOver:    900,
			CreatorRatePer1000Views: &tooMuch,
		},
		Body: "Условия работы.",
	}, uuid.Nil)
	if !errors.Is(err, billing.ErrInvalidInput) {
		t.Fatalf("ожидали отказ по ставке креатора, получили %v", err)
	}
}

// Пустой текст условий тоже не проходит: соглашаться клиенту не с чем.
func TestPublishTermsRequiresBody(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	_, err := svc.PublishTermsVersion(ctx, billing.TermsVersion{
		Terms: billing.Terms{SalaryPerMonth: 6_000_000, RatePer1000Views: 9_000},
		Body:  "   ",
	}, uuid.Nil)
	if !errors.Is(err, billing.ErrInvalidInput) {
		t.Fatalf("ожидали отказ по пустому тексту, получили %v", err)
	}
}

// ---- месяц, который ещё не пересчитывали ----

// Счёт есть до того, как менеджер нажал «Пересчитать».
//
// Раньше непересчитанный месяц выглядел нулём: у заказчика «к оплате
// 0 ₽» при вышедшем ролике на три миллиона просмотров. Данные для счёта
// были все — не было только нажатой кнопки, и заказчик видел ноль там,
// где на самом деле сто с лишним тысяч.
func TestUnrecalculatedMonthShowsComputedTotals(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "prv1", 500_000, true)

	// Пересчёта не было: строк в базе нет.
	var stored int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM creator_accruals WHERE project_id = $1`, pid).Scan(&stored); err != nil {
		t.Fatalf("count accruals: %v", err)
	}
	if stored != 0 {
		t.Fatalf("до пересчёта сохранённых строк быть не должно, их %d", stored)
	}

	out, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing: %v", err)
	}
	if len(out.Accruals) == 0 {
		t.Fatal("месяц с выкладками обязан показывать расчёт, а не пустоту")
	}
	if out.Totals.Total == 0 {
		t.Error("«к оплате» не может быть нулём при вышедшем ролике")
	}
	if out.Totals.Views != 500_000 {
		t.Errorf("просмотры месяца: %d, ожидали 500 000", out.Totals.Views)
	}
	for _, a := range out.Accruals {
		if !a.IsPreview {
			t.Error("строка не сохранена — она обязана быть помечена расчётной")
		}
	}

	// Заказчик видит ровно тот же счёт: он по нему платит.
	cli, err := svc.ClientBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("client billing: %v", err)
	}
	if cli.Totals.Total != out.Totals.Total {
		t.Errorf("у заказчика %d, у менеджера %d — счёт обязан быть один",
			cli.Totals.Total, out.Totals.Total)
	}
}

// Пересчёт превращает расчёт в сохранённые строки — те же числа, но их
// уже можно утвердить и выплатить.
//
// «Предварительно» при этом не снимается: пока месяц идёт, сохранённые
// строки завтра станут другими. Признак снимает только фиксация месяца —
// раньше он зависел от наличия строк в базе, и пересчитанный месяц
// выглядел окончательным, не будучи им.
func TestRecalculateReplacesPreviewWithStoredRows(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "prv2", 800_000, true)

	before, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing: %v", err)
	}
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}
	after, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing after: %v", err)
	}

	if after.Totals.Total != before.Totals.Total {
		t.Errorf("пересчёт изменил счёт: было %d, стало %d",
			before.Totals.Total, after.Totals.Total)
	}
	for _, a := range after.Accruals {
		if !a.IsPreview {
			t.Error("период идёт — строки обязаны оставаться предварительными")
		}
		if a.ID == uuid.Nil {
			t.Error("сохранённой строке нужен id — иначе её нечем утвердить")
		}
	}

	// А вот подытог периода признак снимает.
	period, err := svc.Period(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if _, err := svc.LockPeriod(ctx, period, nil, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("подытог периода: %v", err)
	}
	locked, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("billing after lock: %v", err)
	}
	for _, a := range locked.Accruals {
		if a.IsPreview {
			t.Error("период подытожен — строки больше не предварительные")
		}
	}
}

// Пока не вышел ни один ролик, периодов у проекта нет вовсе — и это
// внятный ответ, а не пустой счёт.
//
// Отсчёт начинается датой первой публикации: до неё отсчитывать не от
// чего, и показывать «за период 1 к оплате 0 ₽» значило бы придумать
// период, которого не было.
func TestProjectWithoutPublicationsHasNoPeriods(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	if _, err := svc.ProjectBilling(ctx, pid, 0, time.Now().UTC()); !errors.Is(err, billing.ErrNoPeriods) {
		t.Fatalf("ожидали ErrNoPeriods, получили %v", err)
	}
	periods, err := svc.Periods(ctx, pid, time.Now().UTC())
	if err != nil {
		t.Fatalf("периоды: %v", err)
	}
	if len(periods) != 0 {
		t.Errorf("периодов %d, ожидали ни одного", len(periods))
	}

	// Вышел первый ролик — появился первый период.
	publishOn(t, pool, pid, creators[0], time.Now().UTC().AddDate(0, 0, -1), "emp01", 1000)
	periods, err = svc.Periods(ctx, pid, time.Now().UTC())
	if err != nil {
		t.Fatalf("периоды: %v", err)
	}
	if len(periods) != 1 || periods[0].Seq != 1 {
		t.Errorf("после первой публикации периодов %d: %+v", len(periods), periods)
	}
}

// Порог, равный нулю, означает «порога нет»: весь объём идёт по ПОЛНОЙ
// ставке.
//
// В SQL это выражается недостижимо большим порогом, а не нулём — ноль
// отправил бы все просмотры в пониженную ступень. Разница между 90 ₽ и
// 9 ₽ за тысячу: счёт занижается примерно вдесятеро, а карточка условий
// при этом выглядит совершенно нормально, потому что в ней та же самая
// полная ставка и написана.
func TestZeroThresholdMeansNoThreshold(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))

	terms := demoTerms(pid)
	terms.BonusViewsThreshold = 0 // порога нет
	if _, err := svc.SaveTerms(ctx, terms, creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	// Два миллиона просмотров — вдвое больше «обычного» порога.
	seedPublicationViews(t, pid, creators[0], pubDay(0), "zt1", 2_000_000, true)

	items, err := recalcCurrent(t, svc, pid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	var bonus int64
	for _, a := range items {
		if a.CreatorUserID == creators[0] {
			bonus = a.ViewsBonus
		}
	}
	// 2 000 000 / 1000 × 90 ₽ = 180 000 ₽ = 18 000 000 копеек.
	const full = 2_000_000 / 1000 * 9_000
	if bonus != full {
		t.Errorf("бонус при нулевом пороге: %d, ожидали %d (весь объём по полной ставке)", bonus, full)
	}
	// Для наглядности: по пониженной ставке вышло бы вдесятеро меньше.
	if bonus == 2_000_000/1000*900 {
		t.Error("весь объём ушёл в пониженную ступень — ноль сработал как порог, а не как его отсутствие")
	}
}

// Смета до заказа и смета по заказу сходятся числом.
//
// Клиент видит сумму на странице подбора — там заказа ещё нет, и она
// считается по составу и объёму. После оформления та же сумма приходит
// уже по заказу. Разъедутся — человек увидит одно до оформления и другое
// после, и поверит второму числу меньше, чем первому. Общий CTE в
// estimate.go заведён ровно ради этого, но проверять его до сих пор было
// нечем: черновая смета не вызывалась ни в одном тесте.
func TestDraftEstimateMatchesOrderEstimate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanupOrders := setupOrderWorld(t, pool)
	defer cleanupOrders()

	// Фикс ЗА РОЛИК выставляем НАМЕРЕННО, и в этом весь тест.
	//
	// Без него обе сметы падали в одну и ту же запасную ветку — оклад за
	// период, — совпадали числом и объявляли себя сошедшимися. А на
	// живом прайсе, где фикс за ролик задан, смета до заказа считала по
	// нему, а смета по заказу по окладу: 30 000 ₽ против 60 000 ₽ за
	// один и тот же заказ. Причина — разные запросы условий: черновая
	// берёт LatestTerms, заказная брала свой укороченный SELECT.
	if _, err := pool.Exec(ctx, `
UPDATE terms_versions SET salary_per_month = 6000000, rate_per_1000_views = 9000,
       bonus_views_threshold = 1000000, rate_per_1000_views_over = 900,
       fee_per_video = 100000, creator_fee_per_video = 50000`); err != nil {
		t.Fatalf("terms: %v", err)
	}

	// Один креатор: в первый месяц клиенту больше и не дадут — правило
	// лимита проверяется отдельно, здесь важно совпадение двух смет.
	const needed, videos = 1, 30
	shortlist := creators[:3]

	svc := billing.NewService(billing.NewRepo(pool))
	draft, err := svc.EstimateDraft(ctx, needed, videos, shortlist)
	if err != nil {
		t.Fatalf("estimate draft: %v", err)
	}

	ordersSvc := orders.NewService(orders.NewRepo(pool))
	res, err := ordersSvc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: needed,
		VideosCount: videos, CreatorIDs: shortlist,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	byOrder, err := svc.EstimateOrder(ctx, res.Order.ID, clientID)
	if err != nil {
		t.Fatalf("estimate order: %v", err)
	}

	if draft.Salaries != byOrder.Salaries {
		t.Errorf("оклады: до заказа %d, по заказу %d", draft.Salaries, byOrder.Salaries)
	}
	if draft.Total != byOrder.Total {
		t.Errorf("итог: до заказа %d, по заказу %d", draft.Total, byOrder.Total)
	}
	if draft.HasForecast != byOrder.HasForecast {
		t.Errorf("наличие прогноза разное: %v против %v", draft.HasForecast, byOrder.HasForecast)
	}
	if draft.WithoutHistory != byOrder.WithoutHistory {
		t.Errorf("людей без истории: до заказа %d, по заказу %d",
			draft.WithoutHistory, byOrder.WithoutHistory)
	}

	// И отдельно — что считалось это именно фиксом за ролик, а не
	// совпало в запасной ветке: тридцать роликов по 1 000 ₽.
	if want := int64(videos) * 100000; draft.Salaries != want {
		t.Errorf("фикс за ролик не применён: %d вместо %d", draft.Salaries, want)
	}

	// Условия в ответе — то, чем смета объясняет свою сумму. Пустой
	// fee_per_video здесь означает подпись «оклад за месяц» под ценой,
	// посчитанной за ролик.
	if byOrder.Terms.FeePerVideo == nil || *byOrder.Terms.FeePerVideo != 100000 {
		t.Errorf("в условиях заказа нет фикса за ролик: %+v", byOrder.Terms.FeePerVideo)
	}
	if draft.Terms.FeePerVideo == nil || *draft.Terms.FeePerVideo != 100000 {
		t.Errorf("в условиях черновой сметы нет фикса за ролик: %+v", draft.Terms.FeePerVideo)
	}
}

// В составе периода у заказчика стоят живые люди, а не буквы в кружках.
//
// Заказчик платит за конкретных креаторов, и открыть страницу того, кто
// снимал, он должен прямо отсюда. Значит, вместе с именем в строку
// начисления обязаны приезжать портрет и адрес страницы.
//
// Отдельная тонкость — ПРЕДВАРИТЕЛЬНАЯ строка: она считается на лету, из
// арифметики, в которой людей нет вовсе. Имя ей дописывает withNames, и
// вместе с именем обязаны дописываться портрет и адрес — иначе до
// первого «Пересчитать» в составе висели бы безымянные кружки без ссылок.
func TestClientAccrualCarriesCreatorProfile(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, avatar_url, username)
VALUES ($1, 'Анастасия Креатор', 'https://cdn.example/nastya.jpg', $2)
ON CONFLICT (user_id) DO UPDATE
SET display_name = EXCLUDED.display_name,
    avatar_url   = EXCLUDED.avatar_url,
    username     = EXCLUDED.username`,
		creators[0], "nastya-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "prof1", 1_100_000, true)

	// В проекте два креатора, и порядок строк здесь не про эту проверку:
	// ищем своего по id, а не по позиции.
	mine := func(t *testing.T, rows []billing.ClientAccrual) billing.ClientAccrual {
		t.Helper()
		for _, a := range rows {
			if a.CreatorUserID == creators[0] {
				return a
			}
		}
		t.Fatalf("строки нужного креатора нет вовсе: %+v", rows)
		return billing.ClientAccrual{}
	}

	// До «Пересчитать»: строка посчитана на лету и людей в себе не несёт.
	preview, err := svc.ClientBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("client billing (preview): %v", err)
	}
	row := mine(t, preview.Accruals)
	if row.CreatorAvatarURL != "https://cdn.example/nastya.jpg" {
		t.Errorf("в предварительной строке нет портрета: %+v", row)
	}
	if row.CreatorUsername == "" {
		t.Errorf("в предварительной строке нет адреса страницы: %+v", row)
	}

	// После сохранения: строка берётся запросом, профиль приезжает тем же
	// выражением, что и имя.
	if _, err := recalcCurrent(t, svc, pid); err != nil {
		t.Fatalf("recalc: %v", err)
	}
	saved, err := svc.ClientBilling(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("client billing (saved): %v", err)
	}
	row = mine(t, saved.Accruals)
	if row.CreatorAvatarURL != "https://cdn.example/nastya.jpg" {
		t.Errorf("в сохранённой строке нет портрета: %+v", row)
	}
	if row.CreatorUsername == "" {
		t.Errorf("в сохранённой строке нет адреса страницы: %+v", row)
	}
}

// Смету спрашивают раньше, чем набран состав.
//
// Ползунок «сколько будет стоить следующий месяц» знает только число
// людей, экран каталога — людей, но не объём роликов. Отказ на такой
// вопрос означает пустое место там, где сервер может ответить точно:
// оклады считаются из числа людей и ставки. Чего не знаем — о том
// говорим прямо: has_forecast=false, а не бонус, равный нулю.
func TestDraftEstimateWithoutRosterAndVolume(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, creators, cleanupOrders := setupOrderWorld(t, pool)
	defer cleanupOrders()

	if _, err := pool.Exec(ctx, `
UPDATE terms_versions SET salary_per_month = 6000000, rate_per_1000_views = 9000,
       bonus_views_threshold = 1000000, rate_per_1000_views_over = 900`); err != nil {
		t.Fatalf("terms: %v", err)
	}
	svc := billing.NewService(billing.NewRepo(pool))

	// Ни состава, ни объёма — только «нужно пятеро».
	bare, err := svc.EstimateDraft(ctx, 5, 0, nil)
	if err != nil {
		t.Fatalf("смета без состава: %v", err)
	}
	if bare.Salaries != 5*6_000_000 {
		t.Errorf("оклады %d, ожидалось 30 000 ₽ × 5", bare.Salaries)
	}
	if bare.Total != bare.Salaries {
		t.Errorf("итог %d, а известны только оклады %d", bare.Total, bare.Salaries)
	}
	if bare.HasForecast {
		t.Error("прогноза бонуса нет — has_forecast обязан быть false")
	}
	if bare.BonusForecast != 0 {
		t.Errorf("бонус %d при неизвестном прогнозе", bare.BonusForecast)
	}

	// Состав есть, объёма нет: считать бонус не из чего, и выдавать
	// ноль за посчитанный нельзя.
	noVolume, err := svc.EstimateDraft(ctx, 3, 0, creators[:3])
	if err != nil {
		t.Fatalf("смета без объёма: %v", err)
	}
	if noVolume.HasForecast {
		t.Error("объём не задан — прогноза быть не может")
	}
	if noVolume.Total != noVolume.Salaries {
		t.Errorf("итог %d, ожидались одни оклады %d", noVolume.Total, noVolume.Salaries)
	}

	// А вот отрицательный объём — это уже ошибка ввода, а не «не знаю».
	if _, err := svc.EstimateDraft(ctx, 1, -1, nil); err == nil {
		t.Error("отрицательный videos_count прошёл")
	}
	// А ноль людей — законный вопрос, и это не послабление.
	//
	// Во второй ветке воронки («видео под ключ») креаторов нет вовсе:
	// снимаем мы, ролики выходят с аккаунтов бренда, и цена там —
	// ролики × фикс. Отказ означал бы пустое место на экране, где
	// сервер отвечает точно. Отрицательное по-прежнему ошибка.
	noCrew, err := svc.EstimateDraft(ctx, 0, 10, nil)
	if err != nil {
		t.Fatalf("смета без людей: %v", err)
	}
	if noCrew.Creators != 0 {
		t.Errorf("в смете без людей креаторов %d", noCrew.Creators)
	}
	if _, err := svc.EstimateDraft(ctx, -1, 10, nil); err == nil {
		t.Error("отрицательный needed прошёл")
	}
}

// Тариф проекта — своя таблица, а не ссылка на прайс площадки.
//
// Правка лесенки действует с ТЕКУЩЕГО периода и видна сразу: суммы
// хранятся строками, и без пересчёта менеджер сохранял бы тариф, видя на
// экране прежние деньги. Подытоженный период при этом не трогается —
// он заморожен вместе со срезом, и по нему уже выставлен счёт.
func TestProjectTariffAppliesForwardOnly(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()
	_ = clientID

	pid, cleanupProject := overviewProject(t, pool, creators[0], 6_000_000, 9_000, 0, "tariff")
	defer cleanupProject()

	svc := billing.NewService(billing.NewRepo(pool))

	// Лесенка: до 300 тыс. просмотров период стоит 300 000 ₽, дальше 450 000.
	fee := func(v int64) *int64 { return &v }
	if _, err := svc.SaveTerms(ctx, billing.Terms{
		ProjectID: pid,
		Steps: []billing.TermsStep{
			{FromViews: 0, ClientFee: 30_000_000, CreatorFee: fee(18_000_000)},
			{FromViews: 300_000, ClientFee: 45_000_000, CreatorFee: fee(28_000_000)},
		},
	}, creators[0]); err != nil {
		t.Fatalf("SaveTerms: %v", err)
	}

	got, err := svc.Terms(ctx, pid)
	if err != nil {
		t.Fatalf("Terms: %v", err)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("ступеней %d, ожидалось две: таблица не сохранилась", len(got.Steps))
	}
	if !got.Stepped() {
		t.Error("тариф со ступенями обязан считаться ступенчатым")
	}

	// Прайс площадки правится отдельно и проект не трогает: это и есть
	// «снимок, а не ссылка».
	if _, err := pool.Exec(ctx, `
UPDATE terms_versions SET salary_per_month = 99000000, rate_per_1000_views = 99000`); err != nil {
		t.Fatalf("правка прайса: %v", err)
	}
	after, err := svc.Terms(ctx, pid)
	if err != nil {
		t.Fatalf("Terms после правки прайса: %v", err)
	}
	if len(after.Steps) != 2 || after.Steps[1].ClientFee != 45_000_000 {
		t.Errorf("прайс площадки переписал тариф проекта: %+v", after.Steps)
	}
}

// Две ступени с одним порогом — неразрешимая неоднозначность: какую
// цену брать, решал бы порядок строк.
func TestProjectTariffRejectsAmbiguousLadder(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()
	pid, cleanupProject := overviewProject(t, pool, creators[0], 6_000_000, 9_000, 0, "tariff2")
	defer cleanupProject()

	svc := billing.NewService(billing.NewRepo(pool))
	fee := func(v int64) *int64 { return &v }
	_, err := svc.SaveTerms(ctx, billing.Terms{
		ProjectID: pid,
		Steps: []billing.TermsStep{
			{FromViews: 300_000, ClientFee: 30_000_000},
			{FromViews: 300_000, ClientFee: 45_000_000},
		},
	}, creators[0])
	if err == nil {
		t.Error("две ступени с одним порогом прошли")
	}

	// И креатору нельзя обещать больше, чем берём с клиента.
	_, err = svc.SaveTerms(ctx, billing.Terms{
		ProjectID: pid,
		Steps:     []billing.TermsStep{{FromViews: 0, ClientFee: 10_000, CreatorFee: fee(20_000)}},
	}, creators[0])
	if err == nil {
		t.Error("ступень с убытком на каждом периоде прошла")
	}
}

// Реестр тарифов: строка — проект.
//
// Раздел «Прайс» показывал общую версию, а вопрос к нему другой: по
// каким условиям идёт вот этот проект и чем он отличается от соседнего.
// Отдельно видно худшее состояние — проект без тарифа: он считается по
// нулям, и на экране денег это выглядит как «ещё не начислили».
func TestTariffRegistryShowsEveryProjectRow(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	withTerms, c1 := overviewProject(t, pool, creators[0], 6_000_000, 9_000, 0, "reg-a")
	defer c1()
	bare, c2 := overviewProject(t, pool, creators[0], 6_000_000, 9_000, 0, "reg-b")
	defer c2()
	// У второго снимок условий снимаем совсем: так выглядит проект,
	// которому тариф не задали.
	if _, err := pool.Exec(ctx, `DELETE FROM project_billing WHERE project_id = $1`, bare); err != nil {
		t.Fatalf("очистка условий: %v", err)
	}

	svc := billing.NewService(billing.NewRepo(pool))
	fee := func(v int64) *int64 { return &v }
	if _, err := svc.SaveTerms(ctx, billing.Terms{
		ProjectID: withTerms,
		Steps: []billing.TermsStep{
			{FromViews: 0, ClientFee: 30_000_000, CreatorFee: fee(18_000_000)},
			{FromViews: 1_000_000, ClientFee: 70_000_000, CreatorFee: fee(45_000_000)},
		},
	}, creators[0]); err != nil {
		t.Fatalf("SaveTerms: %v", err)
	}

	rows, err := svc.TariffRegistry(ctx)
	if err != nil {
		t.Fatalf("TariffRegistry: %v", err)
	}
	byID := map[uuid.UUID]billing.TariffRow{}
	for _, r := range rows {
		byID[r.ProjectID] = r
	}

	a, ok := byID[withTerms]
	if !ok {
		t.Fatalf("проекта с тарифом нет в реестре")
	}
	if !a.Stepped || a.StepsCount != 2 {
		t.Errorf("ступеней %d, ступенчатость %v — ожидалось две ступени", a.StepsCount, a.Stepped)
	}
	// Вилка видна, не открывая проект.
	if a.MinFee != 30_000_000 || a.MaxFee != 70_000_000 {
		t.Errorf("вилка %d..%d, ожидалось 300 000..700 000 ₽", a.MinFee, a.MaxFee)
	}

	b, ok := byID[bare]
	if !ok {
		t.Fatalf("проекта без тарифа нет в реестре — а он самый важный")
	}
	if b.HasTerms {
		t.Error("проект без снимка условий помечен как имеющий тариф")
	}
	if b.StepsCount != 0 {
		t.Errorf("у проекта без тарифа нашлись ступени: %d", b.StepsCount)
	}

	// Проект без тарифа стоит первым: это то, что надо чинить.
	if rows[0].HasTerms {
		t.Error("проекты без тарифа обязаны идти первыми — иначе их не заметят")
	}
}

// Фикс за ролик доезжает до базы ЧЕРЕЗ РУЧКУ.
//
// Ловушка, стоившая целой модели: у ручки свой запрос (termsReq), и
// новое поле надо добавлять в него отдельно. Пока его там не было,
// менеджер сохранял «фикс за ролик» в интерфейсе, сервер отвечал 200 —
// и молча клал в снимок ноль. Ни одна проверка на уровне сервиса этого
// не видела: там число приходило уже разобранным.
func TestSaveTermsKeepsFeePerVideoOverHTTP(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET assigned_to_user_id = $1 WHERE id = $2`, manager, pid); err != nil {
		t.Fatalf("назначение менеджера: %v", err)
	}

	code, _ := h.Do(t, http.MethodPut, "/api/v1/manager/projects/"+pid.String()+"/billing",
		h.Token(t, manager), map[string]any{
			"fee_per_video":         100_000,
			"creator_fee_per_video": 50_000,
			"salary_per_month":      0,
			"rate_per_1000_views":   0,
			"steps": []map[string]any{
				{"from_views": 300_000, "client_fee": 4_500_000},
			},
		})
	if code != http.StatusOK {
		t.Fatalf("сохранение условий: код %d", code)
	}

	terms, err := billing.NewRepo(pool).Terms(context.Background(), pid)
	if err != nil {
		t.Fatalf("Terms: %v", err)
	}
	if terms.FeePerVideo == nil || *terms.FeePerVideo != 100_000 {
		t.Errorf("фикс за ролик в снимке: %v, ожидалось 100000", terms.FeePerVideo)
	}
	if terms.CreatorFeePerVideo == nil || *terms.CreatorFeePerVideo != 50_000 {
		t.Errorf("фикс креатору в снимке: %v, ожидалось 50000", terms.CreatorFeePerVideo)
	}

	// И то же правило, что у остальных креаторских ставок: больше, чем
	// платит заказчик, обещать нельзя — это убыток на каждом ролике.
	code, _ = h.Do(t, http.MethodPut, "/api/v1/manager/projects/"+pid.String()+"/billing",
		h.Token(t, manager), map[string]any{
			"fee_per_video":         100_000,
			"creator_fee_per_video": 200_000,
		})
	if code != http.StatusBadRequest {
		t.Errorf("фикс креатору выше клиентского: код %d, ожидался 400", code)
	}
	_ = creators
}
