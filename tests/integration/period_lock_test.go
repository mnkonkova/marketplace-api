package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/audit"
	"marketpclce/internal/billing"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Период проекта: месяц работы креаторов, отсчитанный от первой
// публикации, а не от первого числа календаря.
//
// Смысл подытога в одном: подытоженный период больше не меняется.
// Раньше утверждение начисления замораживало деньги, но не просмотры —
// числа, из которых выросла сумма, продолжали расти.

// recalcCurrent — пересчёт текущего периода. Вынесен, потому что зовётся
// из большинства денежных тестов, а период теперь надо сперва найти.
func recalcCurrent(t *testing.T, svc *billing.Service, pid uuid.UUID) ([]billing.Accrual, error) {
	t.Helper()
	ctx := context.Background()
	p, err := svc.Period(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return svc.Recalculate(ctx, pid, p)
}

// dueSlot — плановые даты выкладок в тестах должны быть разными: у
// проекта уникальность (креатор, день), и второй ролик на ту же дату
// просто не заведётся. Сама дата плана на период больше не влияет — он
// считается по факту выхода, — поэтому берём любую свободную.
var dueSlot int

func nextDue() time.Time {
	dueSlot++
	return pubDay(dueSlot)
}

// publishOn — выкладка, вышедшая в заданный день.
//
// Дата публикации ставится явно и ссылкам, и моменту сдачи: период
// определяется по факту выхода, а тест не должен зависеть от того,
// когда он сам запустился.
func publishOn(t *testing.T, pool *pgxpool.Pool, pid, creator uuid.UUID, day time.Time, marker string, views int64) uuid.UUID {
	t.Helper()
	pubID := seedPublicationViews(t, pid, creator, nextDue(), marker, views, true)
	if _, err := pool.Exec(context.Background(), `
UPDATE publication_links SET published_at = $2, submitted_at = $2
WHERE publication_id = $1`, pubID, day); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}
	return pubID
}

// submittedOn — выкладка без известной даты публикации: источник её не
// отдал, и остаётся момент сдачи ссылки.
func submittedOn(t *testing.T, pool *pgxpool.Pool, pid, creator uuid.UUID, day time.Time, marker string, views int64) uuid.UUID {
	t.Helper()
	pubID := seedPublicationViews(t, pid, creator, nextDue(), marker, views, true)
	if _, err := pool.Exec(context.Background(), `
UPDATE publication_links SET published_at = NULL, submitted_at = $2
WHERE publication_id = $1`, pubID, day); err != nil {
		t.Fatalf("дата сдачи: %v", err)
	}
	return pubID
}

// Первый период начинается ДАТОЙ ПЕРВОЙ ПУБЛИКАЦИИ — не первым числом
// месяца и не датой создания проекта. Дальше периоды катятся от неё.
func TestFirstPeriodStartsAtFirstPublication(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	// Первый ролик вышел 15-го: периоды должны идти с 15-го по 14-е.
	first := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "per01", 100_000)
	// Второй вышел позже — на границу он не влияет.
	publishOn(t, pool, pid, creators[0], first.AddDate(0, 0, 40), "per02", 200_000)

	periods, err := svc.Periods(ctx, pid, first.AddDate(0, 0, 70))
	if err != nil {
		t.Fatalf("периоды: %v", err)
	}
	if len(periods) < 3 {
		t.Fatalf("периодов %d, ожидали хотя бы три: %+v", len(periods), periods)
	}
	if got := periods[0].StartsOn.Format("2006-01-02"); got != "2026-03-15" {
		t.Errorf("первый период начинается %s, ожидали 2026-03-15 — день первой публикации", got)
	}
	if got := periods[0].EndsOn.Format("2006-01-02"); got != "2026-04-14" {
		t.Errorf("первый период кончается %s, ожидали 2026-04-14", got)
	}
	if periods[0].Seq != 1 || periods[0].PrevPeriodID != nil {
		t.Errorf("у первого периода номер %d и предшественник %v", periods[0].Seq, periods[0].PrevPeriodID)
	}

	// Периоды катятся без дыр и нахлёстов, и каждый знает предыдущий:
	// перенос остатка ступени поедет по этой цепочке.
	for i := 1; i < len(periods); i++ {
		prev, cur := periods[i-1], periods[i]
		if cur.Seq != prev.Seq+1 {
			t.Errorf("номера периодов рвутся: %d после %d", cur.Seq, prev.Seq)
		}
		if want := prev.EndsOn.AddDate(0, 0, 1); !cur.StartsOn.Equal(want) {
			t.Errorf("период %d начинается %s, а предыдущий кончился %s — дыра или нахлёст",
				cur.Seq, cur.StartsOn.Format("2006-01-02"), prev.EndsOn.Format("2006-01-02"))
		}
		if cur.PrevPeriodID == nil || *cur.PrevPeriodID != prev.ID {
			t.Errorf("период %d не ссылается на предыдущий", cur.Seq)
		}
		// Перенос остатка пока нулевой, но место под него есть.
		if cur.CarryInClient != 0 || cur.CarryOutCreator != 0 {
			t.Errorf("перенос остатка заполнен до появления тарифа: %+v", cur)
		}
	}
}

// Ролик, вышедший в последний день периода, принадлежит ЭТОМУ периоду, а
// вышедший на следующий — следующему.
func TestPublicationBelongsToPeriodByPublishDate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	first := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "bnd01", 100_000)
	// Последний день первого периода — 9 июня.
	lastDay := time.Date(2026, 6, 9, 23, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], lastDay, "bnd02", 200_000)
	// Первый день второго — 10 июня.
	nextDay := time.Date(2026, 6, 10, 1, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], nextDay, "bnd03", 400_000)

	p1, err := svc.Period(ctx, pid, 1, nextDay.AddDate(0, 0, 5))
	if err != nil {
		t.Fatalf("первый период: %v", err)
	}
	if got := p1.EndsOn.Format("2006-01-02"); got != "2026-06-09" {
		t.Fatalf("первый период кончается %s, ожидали 2026-06-09", got)
	}
	out, err := svc.ProjectBilling(ctx, pid, 1, nextDay.AddDate(0, 0, 5))
	if err != nil {
		t.Fatalf("биллинг первого периода: %v", err)
	}
	if out.Totals.Views != 300_000 {
		t.Errorf("в первом периоде %d просмотров, ожидали 300000 (два ролика по границу включительно)",
			out.Totals.Views)
	}
	out, err = svc.ProjectBilling(ctx, pid, 2, nextDay.AddDate(0, 0, 5))
	if err != nil {
		t.Fatalf("биллинг второго периода: %v", err)
	}
	if out.Totals.Views != 400_000 {
		t.Errorf("во втором периоде %d просмотров, ожидали 400000 (ролик следующего дня)",
			out.Totals.Views)
	}
}

// Ролик без известной даты публикации попадает по дате сдачи ссылки.
// Плановую due_date не используем вовсе: это план, а не факт.
func TestPublicationWithoutPublishDateFallsBackToSubmitted(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "sub01", 100_000)
	// Источник даты не отдал: считаем по сдаче — во втором периоде.
	submittedOn(t, pool, pid, creators[0], first.AddDate(0, 1, 5), "sub02", 500_000)

	out, err := svc.ProjectBilling(ctx, pid, 2, first.AddDate(0, 2, 0))
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	if out.Totals.Views != 500_000 {
		t.Errorf("во втором периоде %d просмотров, ожидали 500000 — ролик лёг по дате сдачи",
			out.Totals.Views)
	}
}

// Главный тест: подытоженный период не меняется.
func TestLockedPeriodDoesNotChange(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "lck01", 0)
	cutoff := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 14)
	resetDailyViews(t, pool, pid)
	seedDailyViews(t, pool, pid, cutoff.AddDate(0, 0, -3), 500_000)

	p1, err := svc.Period(ctx, pid, 1, cutoff)
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	before, err := svc.ProjectBilling(ctx, pid, 1, cutoff)
	if err != nil {
		t.Fatalf("биллинг до подытога: %v", err)
	}
	if before.Period.Status != billing.PeriodOpen || before.Totals.Views != 500_000 {
		t.Fatalf("до подытога: статус %q, просмотры %d", before.Period.Status, before.Totals.Views)
	}

	locked, err := svc.LockPeriod(ctx, p1, nil, cutoff, cutoff)
	if err != nil {
		t.Fatalf("подытог: %v", err)
	}
	if !locked.IsLocked() || locked.LockedAt == nil {
		t.Fatalf("период не подытожился: %+v", locked)
	}

	// Ролик продолжает набирать просмотры.
	seedDailyViews(t, pool, pid, cutoff.AddDate(0, 0, 10), 9_000_000)

	after, err := svc.ProjectBilling(ctx, pid, 1, cutoff.AddDate(0, 0, 20))
	if err != nil {
		t.Fatalf("биллинг после подытога: %v", err)
	}
	if after.Totals.Views != before.Totals.Views || after.Totals.Total != before.Totals.Total {
		t.Errorf("подытоженный период изменился: просмотры %d→%d, счёт %d→%d",
			before.Totals.Views, after.Totals.Views, before.Totals.Total, after.Totals.Total)
	}
	assertAllPreview(t, after.Accruals, false, "период подытожен")

	// И событие в чат ушло — это единственный момент, когда по периоду
	// становится что обсуждать.
	var events int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = 'project.period_closed'`,
		pid.String()).Scan(&events); err != nil {
		t.Fatalf("события: %v", err)
	}
	if events != 1 {
		t.Errorf("событий о подытоге %d, ожидали одно", events)
	}
}

// Отсечка — конец периода плюс 14 дней; опоздание фоновой задачи на
// числа не влияет.
func TestPeriodCutoffIgnoresWorkerDelay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Date(2026, 2, 8, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "cut02", 0)
	periodEnd := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	cutoff := periodEnd.AddDate(0, 0, 15) // конец + 1 день + 14 дней отсрочки

	resetDailyViews(t, pool, pid)
	seedDailyViews(t, pool, pid, cutoff.AddDate(0, 0, -2), 300_000)
	seedDailyViews(t, pool, pid, cutoff.AddDate(0, 0, 4), 2_500_000)

	// Фоновая задача опоздала на одиннадцать дней.
	late := cutoff.AddDate(0, 0, 11)
	locked, failed, err := svc.LockDuePeriods(ctx, late, billing.DefaultPeriodLockDelay)
	if err != nil {
		t.Fatalf("фоновой подытог: %v", err)
	}
	if locked == 0 || failed > 0 {
		t.Fatalf("фоновой подытог: закрыто %d, сбоев %d", locked, failed)
	}
	out, err := svc.ProjectBilling(ctx, pid, 1, late)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	if out.Totals.Views != 300_000 {
		t.Errorf("период запомнил %d просмотров, ожидали 300000 — значение на отсечку", out.Totals.Views)
	}
	if out.Period.SnapshotAsOf == nil || !sameDay(*out.Period.SnapshotAsOf, cutoff) {
		t.Errorf("отсечка %v, ожидали %v", out.Period.SnapshotAsOf, cutoff)
	}
	// Период, который ещё не кончился, фоновая задача не трогает.
	if p2, err := svc.Period(ctx, pid, 2, late); err == nil && p2.IsLocked() {
		t.Error("подытожен период, срок которого ещё не вышел")
	}
}

// Ручного подытога больше нет: только автоматика.
func TestManualLockEndpointIsGone(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	code, _ := h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+uuid.NewString()+"/billing/lock_month", h.Token(t, manager), nil)
	if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("ручка ручной фиксации отвечает %d — её не должно быть вовсе", code)
	}
}

// «Предварительно» снимается именно подытогом.
func TestPreviewFlagFollowsPeriodState(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	publishOn(t, pool, pid, creators[0], first, "prv01", 300_000)
	now := first.AddDate(0, 0, 10)

	out, err := svc.ProjectBilling(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, true, "период идёт, ничего не пересчитано")

	p1, err := svc.Period(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if _, err := svc.Recalculate(ctx, pid, p1); err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	out, err = svc.ProjectBilling(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, true, "пересчитано, но период не подытожен")

	if _, err := svc.LockPeriod(ctx, p1, nil, first.AddDate(0, 1, 14), time.Now().UTC()); err != nil {
		t.Fatalf("подытог: %v", err)
	}
	out, err = svc.ProjectBilling(ctx, pid, 1, now)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, false, "период подытожен")
}

// Чистильщик статистики не сносит срез подытоженного периода — и не
// схлопывает ряд, пока у проекта есть неподытоженный период.
func TestCollapseKeepsLockedPeriodSnapshot(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	bsvc := billing.NewService(billing.NewRepo(pool))
	prepo := publications.NewRepo(pool)
	if _, err := bsvc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Now().UTC().AddDate(0, -2, 0)
	publishOn(t, pool, pid, creators[0], first, "clp01", 600_000)

	if _, err := pool.Exec(ctx,
		`UPDATE projects SET collection_stops_at = now() - interval '1 day' WHERE id = $1`, pid); err != nil {
		t.Fatalf("закрыть сбор: %v", err)
	}
	if n, err := prepo.CollapseFinished(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("схлопывание: %v", err)
	} else if n != 0 {
		t.Errorf("схлопнули проект с неподытоженным периодом (%d)", n)
	}

	// Подытоживаем всё, чему пора.
	if _, _, err := bsvc.LockDuePeriods(ctx, time.Now().UTC(), billing.DefaultPeriodLockDelay); err != nil {
		t.Fatalf("подытог: %v", err)
	}
	before, err := bsvc.ProjectBilling(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}

	if n, err := prepo.CollapseFinished(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("схлопывание: %v", err)
	} else if n != 1 {
		t.Fatalf("проект с подытоженными периодами не схлопнулся (%d)", n)
	}
	if daily := dailyRows(t, pool, pid); daily != 0 {
		t.Errorf("ежедневный ряд остался после схлопывания (%d строк)", daily)
	}
	if snap := periodSnapshotRows(t, pool, pid); snap == 0 {
		t.Fatal("чистильщик снёс срез подытоженного периода")
	}
	after, err := bsvc.ProjectBilling(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("биллинг после чистки: %v", err)
	}
	if after.Totals.Views != before.Totals.Views {
		t.Errorf("после чистки период изменился: было %d, стало %d",
			before.Totals.Views, after.Totals.Views)
	}
}

// Переоткрытие — только админу и со следом в журнале.
func TestUnlockPeriodIsAdminOnlyAndLogged(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Now().UTC().AddDate(0, -2, 0)
	publishOn(t, pool, pid, creators[0], first, "unl01", 800_000)
	if _, _, err := svc.LockDuePeriods(ctx, time.Now().UTC(), billing.DefaultPeriodLockDelay); err != nil {
		t.Fatalf("подытог: %v", err)
	}

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	defer cleanupAdmin()
	defer func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'project' AND object_id = $1`, pid.String())
	}()

	path := "/api/v1/admin/projects/" + pid.String() + "/billing/unlock_period?period=1"
	if code, _ := h.Do(t, http.MethodPost, path, h.Token(t, manager), nil); code != http.StatusForbidden {
		t.Fatalf("менеджер переоткрыл период: код %d, ожидали 403", code)
	}
	p1, err := svc.Period(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if !p1.IsLocked() {
		t.Fatal("отказ всё-таки переоткрыл период")
	}

	code, body := h.Do(t, http.MethodPost, path, h.Token(t, admin),
		map[string]any{"reason": "ошиблись периодом"})
	if code != http.StatusOK {
		t.Fatalf("админ не смог переоткрыть: код %d, тело %v", code, body)
	}
	p1, err = svc.Period(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if p1.IsLocked() {
		t.Error("период остался подытоженным")
	}
	if snap := periodSnapshotRows(t, pool, pid); snap != 0 {
		t.Errorf("срез пережил переоткрытие (%d строк)", snap)
	}
	if n := auditCountAction(t, pool, audit.ActionPeriodUnlock, pid.String()); n != 1 {
		t.Errorf("записей в журнале %d, ожидали 1", n)
	}
}

// ---- помощники ----

func assertAllPreview(t *testing.T, items []billing.Accrual, want bool, when string) {
	t.Helper()
	if len(items) == 0 {
		t.Fatalf("%s: строк нет вовсе", when)
	}
	for _, a := range items {
		if a.IsPreview != want {
			t.Errorf("%s: is_preview=%v, ожидали %v", when, a.IsPreview, want)
			return
		}
	}
}

// resetDailyViews — очистить поденный ряд проекта.
func resetDailyViews(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
DELETE FROM video_stat_daily
WHERE link_id IN (
    SELECT l.id FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    WHERE p.project_id = $1
)`, pid); err != nil {
		t.Fatalf("очистить ряд: %v", err)
	}
}

// seedDailyViews — строка ряда за конкретный день на одной площадке
// проекта. Одной достаточно: порог считается по ролику целиком.
func seedDailyViews(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, day time.Time, views int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, collected_at)
SELECT l.id, $2::date, $3, 0, 0, $2::timestamptz
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1
ORDER BY l.platform
LIMIT 1
ON CONFLICT (link_id, stat_date) DO UPDATE SET views = EXCLUDED.views`,
		pid, day, views); err != nil {
		t.Fatalf("строка ряда за %s: %v", day.Format("2006-01-02"), err)
	}
}

func dailyRows(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1`, pid).Scan(&n); err != nil {
		t.Fatalf("считать ежедневные строки: %v", err)
	}
	return n
}

func periodSnapshotRows(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM project_period_views v
JOIN project_periods p ON p.id = v.period_id
WHERE p.project_id = $1`, pid).Scan(&n); err != nil {
		t.Fatalf("считать срез: %v", err)
	}
	return n
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// Поденный ряд удалён — период всё равно подытоживается, но помечен
// приблизительным, и сохранённые суммы при этом не обнуляются.
//
// Покрытие на это жило в тестах месяца и при переезде на периоды не
// переехало: греп по tests/ не находил ни одного snapshot_approx.
func TestApproximateSnapshotWhenSeriesCollapsed(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	first := time.Now().UTC().AddDate(0, -2, 0)
	publishOn(t, pool, pid, creators[0], first, "apx02", 650_000)

	// Считаем период, пока данные ещё есть: именно эти суммы и должны
	// пережить чистку.
	p1, err := svc.Period(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if _, err := svc.Recalculate(ctx, pid, p1); err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	before, err := svc.ProjectBilling(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	if before.Totals.Total == 0 {
		t.Fatal("до чистки счёт нулевой — тест ничего не проверит")
	}

	// Чистильщик уже схлопнул ряд: итоговый снимок есть, поденных строк нет.
	if _, err := pool.Exec(ctx, `
INSERT INTO project_stat_summary (project_id, views, likes, comments, videos_count, as_of, collapsed_at)
VALUES ($1, 650000, 0, 0, 1, CURRENT_DATE, now())
ON CONFLICT (project_id) DO NOTHING`, pid); err != nil {
		t.Fatalf("итоговый снимок: %v", err)
	}
	resetDailyViews(t, pool, pid)

	locked, err := svc.LockPeriod(ctx, p1, nil, p1.LockDueAt(billing.DefaultPeriodLockDelay), time.Now().UTC())
	if err != nil {
		t.Fatalf("подытог: %v", err)
	}
	if !locked.IsLocked() {
		t.Fatal("период не подытожился — открытым навсегда он остаться не должен")
	}
	if !locked.SnapshotApprox {
		t.Fatal("срез снят с пустоты, а приблизительным не помечен")
	}

	after, err := svc.ProjectBilling(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("биллинг после подытога: %v", err)
	}
	if after.Totals.Total != before.Totals.Total {
		t.Errorf("подытог обнулил посчитанные суммы: было %d, стало %d",
			before.Totals.Total, after.Totals.Total)
	}
	if !after.Period.SnapshotApprox {
		t.Error("приблизительность не доехала до ответа менеджера")
	}

	// И до заказчика тоже: сводка, часть чисел которой подтянута,
	// обязана об этом сказать.
	client, err := svc.ClientBilling(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("биллинг заказчика: %v", err)
	}
	if !client.Period.SnapshotApprox {
		t.Error("заказчик не видит, что числа приблизительные")
	}
}

// Подытог снимает ролики периода с обхода: числа заморожены срезом, и
// дальше каждый обход — кредит поставщика впустую. Переоткрытие
// возвращает их в очередь: пересчитывать иначе будет нечего.
func TestLockStopsCollectingPeriodLinks(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	// Два ролика в РАЗНЫХ периодах: первый — в том, который подытожим,
	// второй — в следующем. Без второго тест был бы зелёным и у запроса
	// без всякого условия по периоду, то есть проверял бы не правило, а
	// сам факт записи.
	first := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	inPeriod := publishOn(t, pool, pid, creators[0], first, "stop01", 0)
	next := time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC)
	nextPeriod := publishOn(t, pool, pid, creators[0], next, "stop02", 0)
	cutoff := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 14)

	// Сколько ссылок этой выкладки ещё в очереди. Считаем по конкретному
	// ролику, а не по проекту: правило именно про ролики периода.
	due := func(pub uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM publication_links
WHERE publication_id = $1 AND next_collect_at <> 'infinity'::timestamptz`, pub).Scan(&n); err != nil {
			t.Fatalf("ссылки в очереди: %v", err)
		}
		return n
	}

	if due(inPeriod) == 0 || due(nextPeriod) == 0 {
		t.Fatal("до подытога ссылки уже сняты с обхода")
	}

	p1, err := svc.Period(ctx, pid, 1, cutoff)
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if _, err := svc.LockPeriod(ctx, p1, nil, cutoff, cutoff); err != nil {
		t.Fatalf("подытог: %v", err)
	}
	if n := due(inPeriod); n != 0 {
		t.Errorf("после подытога у ролика периода осталось %d ссылок, ожидали ноль", n)
	}
	// Ролик следующего периода трогать нельзя: его период ещё идёт.
	if due(nextPeriod) == 0 {
		t.Error("подытог снял с обхода и ролик чужого периода")
	}

	locked, err := svc.Period(ctx, pid, 1, cutoff)
	if err != nil {
		t.Fatalf("период после подытога: %v", err)
	}
	if _, err := svc.UnlockPeriod(ctx, locked.ID, creators[0], "пересчитать", cutoff); err != nil {
		t.Fatalf("переоткрытие: %v", err)
	}
	if due(inPeriod) == 0 {
		t.Error("после переоткрытия ссылки не вернулись в очередь")
	}

	// И правка адреса у снятой с обхода ссылки не возвращает её в
	// очередь: подытог для её периода уже был и больше не повторится,
	// снять её снова будет некому.
	//
	// Период перечитываем: в руках лежит снимок ДО переоткрытия, а
	// Service.LockPeriod по нему решает, что подытоживать уже нечего.
	reopened, err := svc.Period(ctx, pid, 1, cutoff)
	if err != nil {
		t.Fatalf("период после переоткрытия: %v", err)
	}
	if _, err := svc.LockPeriod(ctx, reopened, nil, cutoff, cutoff); err != nil {
		t.Fatalf("повторный подытог: %v", err)
	}
	if n := due(inPeriod); n != 0 {
		t.Fatalf("повторный подытог не снял ссылки с обхода: осталось %d", n)
	}
	pubSvc := publications.NewService(publications.NewRepo(pool))
	if _, err := pubSvc.CreatorEditLink(ctx, publications.ManagerEditLinkInput{
		PublicationID: inPeriod,
		ManagerUserID: creators[0],
		Platform:      "tiktok",
		URL:           "https://www.tiktok.com/@u/video/777",
	}); err != nil {
		t.Fatalf("правка ссылки: %v", err)
	}
	if n := due(inPeriod); n != 0 {
		t.Errorf("правка адреса вернула в очередь %d ссылок подытоженного периода", n)
	}
}

// fakeStats — вместо publications.Service в подытоге: считает, что его
// позвали, и чем ответить.
type fakeStats struct {
	refreshed  []uuid.UUID
	refreshErr error
}

func (f *fakeStats) ForceRefreshProject(
	_ context.Context, projectID uuid.UUID, _ time.Time,
) (publications.CollectStats, error) {
	f.refreshed = append(f.refreshed, projectID)
	return publications.CollectStats{Saved: 1}, f.refreshErr
}

// Подытог форсированно обходит ролики ДО снятия среза.
//
// Обычный сбор идёт по заходу в кабинет. Если в последний день периода
// карточку никто не открыл, срез снялся бы с позавчерашних цифр — и по
// ним выставили бы счёт. Этот проход единственный, который не зависит от
// того, смотрит ли кто-то на экран.
//
// Снятие роликов с обхода после подытога проверяется отдельно
// (TestCollectStopsAfterPeriodLock): его делает parkPeriodLinks в той же
// транзакции, что и срез.
func TestLockPeriodForcesRefreshBeforeSnapshot(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	fake := &fakeStats{}
	svc := billing.NewService(billing.NewRepo(pool)).WithStats(fake)
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("SaveTerms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], nextDue(), "lockpark1", 100_000, true)

	p, err := svc.Period(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("Period: %v", err)
	}
	locked, err := svc.LockPeriod(ctx, p, nil, p.EndsOn, time.Now().UTC())
	if err != nil {
		t.Fatalf("LockPeriod: %v", err)
	}
	if !locked.IsLocked() {
		t.Fatal("период не подытожен")
	}
	if len(fake.refreshed) != 1 || fake.refreshed[0] != pid {
		t.Errorf("форсированный обход: %v, ожидался один по этому проекту", fake.refreshed)
	}

	// Повторный подытог идемпотентен и ничего не делает заново: иначе
	// каждый тик воркера платил бы за обход закрытого периода.
	if _, err := svc.LockPeriod(ctx, locked, nil, locked.EndsOn, time.Now().UTC()); err != nil {
		t.Fatalf("LockPeriod (повтор): %v", err)
	}
	if len(fake.refreshed) != 1 {
		t.Errorf("повторный подытог обошёл проект снова: %v", fake.refreshed)
	}
}

// Недоступный сборщик подытог не останавливает.
//
// Подытог — событие по календарю, и остановить его из-за упавшего
// сборщика значит остановить счета всем проектам сразу. Срез тогда
// снимается с тех цифр, что есть, — ровно прежнее поведение, — а об
// ошибке сказано возвратом, чтобы она попала в лог воркера.
func TestLockPeriodSurvivesCollectorOutage(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	fake := &fakeStats{refreshErr: errors.New("сборщик недоступен")}
	svc := billing.NewService(billing.NewRepo(pool)).WithStats(fake)
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("SaveTerms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], nextDue(), "lockfail1", 50_000, true)

	p, err := svc.Period(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("Period: %v", err)
	}
	locked, err := svc.LockPeriod(ctx, p, nil, p.EndsOn, time.Now().UTC())
	if err == nil {
		t.Error("об ошибке сборщика не сказано — она пропала бы из логов")
	}
	if !locked.IsLocked() {
		t.Fatal("подытог не состоялся из-за сборщика — счета встали бы всем проектам")
	}
	// И ролики периода всё равно сняты с обхода: срез снят, счёт
	// посчитан — считать по ним больше нечего. Это делает подытог сам, в
	// своей транзакции, а не сборщик.
	var live int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM publication_links l
JOIN project_period_publications spp ON spp.publication_id = l.publication_id
WHERE spp.period_id = $1 AND l.next_collect_at <> 'infinity'::timestamptz`, locked.ID).
		Scan(&live); err != nil {
		t.Fatalf("read links: %v", err)
	}
	if live != 0 {
		t.Errorf("%d ссылок остались в обходе, хотя период подытожен", live)
	}
}

// Переоткрытие периода возвращает ролики в обход.
//
// Админ вернул период в работу — значит, по нему будут считать заново.
// Без возврата это означало бы «считайте по мёртвым цифрам», и заметили
// бы это на втором счёте. resumePeriodLinks для этого и написан; тест
// сторожит именно его вызов — функция, которую забыли позвать,
// выглядит как рабочая.
func TestUnlockPeriodReopensLinks(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	fake := &fakeStats{}
	svc := billing.NewService(billing.NewRepo(pool)).WithStats(fake)
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("SaveTerms: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], nextDue(), "unlockpark1", 70_000, true)

	p, err := svc.Period(ctx, pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("Period: %v", err)
	}
	locked, err := svc.LockPeriod(ctx, p, nil, p.EndsOn, time.Now().UTC())
	if err != nil {
		t.Fatalf("LockPeriod: %v", err)
	}

	if _, err := svc.UnlockPeriod(ctx, locked.ID, creators[0], "пересчёт по жалобе",
		time.Now().UTC()); err != nil {
		t.Fatalf("UnlockPeriod: %v", err)
	}
	var live int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM publication_links
WHERE publication_id IN (
    SELECT id FROM project_publications WHERE project_id = $1
) AND next_collect_at <> 'infinity'::timestamptz`, pid).Scan(&live); err != nil {
		t.Fatalf("read links: %v", err)
	}
	if live == 0 {
		t.Error("после переоткрытия ссылки остались снятыми — считали бы по мёртвым цифрам")
	}
}
