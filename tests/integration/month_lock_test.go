package integration_test

import (
	"context"
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

// Месяц проекта: идёт или зафиксирован.
//
// Смысл фиксации в одном: зафиксированный месяц больше не меняется.
// Раньше утверждение начисления замораживало деньги, но не просмотры —
// числа, из которых выросла сумма, продолжали расти, и через полгода
// рядом с прежней суммой стояли другие просмотры.

// Главный тест: зафиксированный месяц не меняется, что бы ни случилось с
// живыми просмотрами.
func TestLockedMonthDoesNotChange(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "lock01", 500_000, true)

	month := time.Now().UTC()
	before, err := svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг до фиксации: %v", err)
	}
	if before.Month.Status != billing.MonthOpen {
		t.Fatalf("месяц должен идти, а он %q", before.Month.Status)
	}
	if before.Totals.Views != 500_000 {
		t.Fatalf("просмотры до фиксации %d, ожидали 500000", before.Totals.Views)
	}

	locked, err := svc.LockMonth(ctx, pid, month, &creators[0], time.Now().UTC())
	if err != nil {
		t.Fatalf("фиксация: %v", err)
	}
	if locked.Status != billing.MonthLocked || locked.LockedAt == nil {
		t.Fatalf("месяц не зафиксировался: %+v", locked)
	}

	// Просмотры продолжают расти — ролик живёт своей жизнью.
	bumpViews(t, pool, pid, 9_000_000)

	after, err := svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг после фиксации: %v", err)
	}
	if after.Totals.Views != before.Totals.Views {
		t.Errorf("просмотры зафиксированного месяца изменились: было %d, стало %d",
			before.Totals.Views, after.Totals.Views)
	}
	if after.Totals.Total != before.Totals.Total {
		t.Errorf("счёт зафиксированного месяца изменился: было %d, стало %d",
			before.Totals.Total, after.Totals.Total)
	}

	// И пересчёт зафиксированного месяца тоже считает по срезу, а не по
	// живым числам: кнопку «Пересчитать» никто не убирал.
	if _, err := svc.Recalculate(ctx, pid, month); err != nil {
		t.Fatalf("пересчёт после фиксации: %v", err)
	}
	again, err := svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг после пересчёта: %v", err)
	}
	if again.Totals.Views != before.Totals.Views || again.Totals.Total != before.Totals.Total {
		t.Errorf("пересчёт переписал зафиксированный месяц: просмотры %d, счёт %d",
			again.Totals.Views, again.Totals.Total)
	}
}

// «Предварительно» снимается именно фиксацией, а не пересчётом.
//
// Раньше признак зависел от того, есть ли сохранённые строки: нажал
// «Пересчитать» — и месяц выглядел окончательным, хотя завтра числа
// станут другими.
func TestPreviewFlagFollowsMonthState(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "prev01", 300_000, true)
	month := time.Now().UTC()

	// Ничего не считали — строки предварительные.
	out, err := svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, true, "месяц идёт, ничего не пересчитано")

	// Пересчитали — строки в базе есть, но месяц всё ещё идёт.
	if _, err := svc.Recalculate(ctx, pid, month); err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	out, err = svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, true, "пересчитано, но месяц не зафиксирован")

	// Зафиксировали — теперь числа окончательные.
	if _, err := svc.LockMonth(ctx, pid, month, &creators[0], time.Now().UTC()); err != nil {
		t.Fatalf("фиксация: %v", err)
	}
	out, err = svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, false, "месяц зафиксирован")
}

// Автоматическая фиксация срабатывает на 14-й день после конца месяца и
// не срабатывает на 13-й.
func TestAutoLockFiresOnDayFourteen(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	// Выкладка в прошлом месяце: считаем от его конца.
	lastMonth := firstOfThisMonth().AddDate(0, -1, 0)
	seedPublicationViews(t, pid, creators[0], lastMonth.AddDate(0, 0, 3), "auto01", 200_000, true)
	monthEnd := lastMonth.AddDate(0, 1, 0)

	// Тринадцатый день — рано.
	if _, _, err := svc.LockDueMonths(ctx, monthEnd.AddDate(0, 0, 13), billing.DefaultMonthLockDelay); err != nil {
		t.Fatalf("проход на 13-й день: %v", err)
	}
	m, err := svc.Month(ctx, pid, lastMonth)
	if err != nil {
		t.Fatalf("состояние месяца: %v", err)
	}
	if m.IsLocked() {
		t.Fatalf("месяц закрылся на 13-й день — правило «через 14 дней» не соблюдено")
	}

	// Четырнадцатый — пора.
	if _, _, err := svc.LockDueMonths(ctx, monthEnd.AddDate(0, 0, 14), billing.DefaultMonthLockDelay); err != nil {
		t.Fatalf("проход на 14-й день: %v", err)
	}
	m, err = svc.Month(ctx, pid, lastMonth)
	if err != nil {
		t.Fatalf("состояние месяца: %v", err)
	}
	if !m.IsLocked() {
		t.Fatalf("месяц не закрылся на 14-й день: %+v", m)
	}
	// Фоновая фиксация — не человек: в locked_by пусто, и приписывать её
	// кому-то было бы враньём.
	if m.LockedBy != nil {
		t.Errorf("фоновая фиксация записалась на человека %v", m.LockedBy)
	}

	// Повторный проход ничего не портит.
	viewsBefore := totalViews(t, svc, pid, lastMonth)
	if _, _, err := svc.LockDueMonths(ctx, monthEnd.AddDate(0, 0, 20), billing.DefaultMonthLockDelay); err != nil {
		t.Fatalf("повторный проход: %v", err)
	}
	again, err := svc.Month(ctx, pid, lastMonth)
	if err != nil {
		t.Fatalf("состояние месяца: %v", err)
	}
	if !again.LockedAt.Equal(*m.LockedAt) {
		t.Errorf("повторная фиксация переписала отметку: было %v, стало %v", m.LockedAt, again.LockedAt)
	}
	if got := totalViews(t, svc, pid, lastMonth); got != viewsBefore {
		t.Errorf("повторная фиксация изменила числа: было %d, стало %d", viewsBefore, got)
	}
}

// Повторная фиксация руками — тоже не ошибка и ничего не меняет.
func TestLockMonthIsIdempotent(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "idem01", 400_000, true)
	month := time.Now().UTC()

	first, err := svc.LockMonth(ctx, pid, month, &creators[0], time.Now().UTC())
	if err != nil {
		t.Fatalf("первая фиксация: %v", err)
	}
	bumpViews(t, pool, pid, 7_000_000)
	second, err := svc.LockMonth(ctx, pid, month, &creators[0], time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("повторная фиксация: %v", err)
	}
	if !second.LockedAt.Equal(*first.LockedAt) {
		t.Errorf("повторная фиксация переписала отметку времени")
	}
	if got := totalViews(t, svc, pid, month); got != 400_000 {
		t.Errorf("повторная фиксация пересняла срез: просмотры %d, ожидали 400000", got)
	}
}

// Чистильщик статистики не сносит зафиксированный срез — и не схлопывает
// ряд, пока у проекта есть незакрытый месяц.
func TestCollapseKeepsLockedSnapshot(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	bsvc := billing.NewService(billing.NewRepo(pool))
	prepo := publications.NewRepo(pool)
	if _, err := bsvc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "coll01", 600_000, true)
	month := time.Now().UTC()

	// Проект закрыт, срок сбора вышел.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET collection_stops_at = now() - interval '1 day' WHERE id = $1`, pid); err != nil {
		t.Fatalf("закрыть сбор: %v", err)
	}
	// Пока месяц не зафиксирован, схлопывать нечего: числа месяца ещё
	// живые, и снимать срез будет неоткуда.
	if n, err := prepo.CollapseFinished(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("схлопывание: %v", err)
	} else if n != 0 {
		t.Errorf("схлопнули проект с незакрытым месяцем (%d)", n)
	}
	if daily := dailyRows(t, pool, pid); daily == 0 {
		t.Fatal("ежедневный ряд удалён до фиксации месяца")
	}

	if _, err := bsvc.LockMonth(ctx, pid, month, &creators[0], time.Now().UTC()); err != nil {
		t.Fatalf("фиксация: %v", err)
	}
	before := totalViews(t, bsvc, pid, month)

	if n, err := prepo.CollapseFinished(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("схлопывание: %v", err)
	} else if n != 1 {
		t.Fatalf("проект с зафиксированным месяцем не схлопнулся (%d)", n)
	}
	if daily := dailyRows(t, pool, pid); daily != 0 {
		t.Errorf("ежедневный ряд остался после схлопывания (%d строк)", daily)
	}
	// Вот ради чего всё: срез пережил чистку, и месяц читается прежним.
	if snap := snapshotRows(t, pool, pid); snap == 0 {
		t.Fatal("чистильщик снёс срез зафиксированного месяца")
	}
	if got := totalViews(t, bsvc, pid, month); got != before {
		t.Errorf("после чистки месяц изменился: было %d, стало %d", before, got)
	}
}

// Расфиксация — только админу и со следом в журнале.
func TestUnlockMonthIsAdminOnlyAndLogged(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creators[0], pubDay(0), "unlk01", 800_000, true)
	month := time.Now().UTC()
	if _, err := svc.LockMonth(ctx, pid, month, &creators[0], time.Now().UTC()); err != nil {
		t.Fatalf("фиксация: %v", err)
	}

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	defer cleanupAdmin()
	defer func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'project' AND object_id = $1`, pid.String())
	}()

	path := "/api/v1/admin/projects/" + pid.String() + "/billing/unlock_month?month=" +
		month.Format("2006-01")

	if code, _ := h.Do(t, http.MethodPost, path, h.Token(t, manager), nil); code != http.StatusForbidden {
		t.Fatalf("менеджер расфиксировал месяц: код %d, ожидали 403", code)
	}
	if m, err := svc.Month(ctx, pid, month); err != nil || !m.IsLocked() {
		t.Fatalf("отказ всё-таки расфиксировал месяц: %+v (%v)", m, err)
	}

	code, body := h.Do(t, http.MethodPost, path, h.Token(t, admin),
		map[string]any{"reason": "ошиблись месяцем"})
	if code != http.StatusOK {
		t.Fatalf("админ не смог расфиксировать: код %d, тело %v", code, body)
	}
	m, err := svc.Month(ctx, pid, month)
	if err != nil {
		t.Fatalf("состояние месяца: %v", err)
	}
	if m.IsLocked() {
		t.Error("месяц остался зафиксированным")
	}
	if snap := snapshotRows(t, pool, pid); snap != 0 {
		t.Errorf("срез пережил расфиксацию (%d строк) — рядом с живым расчётом это второй ответ на тот же вопрос", snap)
	}
	if n := auditCountAction(t, pool, audit.ActionMonthUnlock, pid.String()); n != 1 {
		t.Errorf("записей в журнале %d, ожидали 1", n)
	}
	// После расфиксации месяц снова идёт: строки опять помечены
	// «предварительно», а числа станут живыми на ближайшем пересчёте.
	// Сами по себе они не меняются — в базе лежит то, что посчитали при
	// фиксации, и молча переписывать это расфиксация не должна.
	bumpViews(t, pool, pid, 5_000_000)
	out, err := svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	assertAllPreview(t, out.Accruals, true, "месяц расфиксирован")
	if out.Totals.Views != 800_000 {
		t.Errorf("расфиксация сама переписала числа: %d", out.Totals.Views)
	}
	if _, err := svc.Recalculate(ctx, pid, month); err != nil {
		t.Fatalf("пересчёт после расфиксации: %v", err)
	}
	out, err = svc.ProjectBilling(ctx, pid, month)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	if out.Totals.Views != 5_000_000 {
		t.Errorf("после расфиксации и пересчёта месяц не вернулся к живым числам: %d",
			out.Totals.Views)
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

func totalViews(t *testing.T, svc *billing.Service, pid uuid.UUID, month time.Time) int64 {
	t.Helper()
	out, err := svc.ProjectBilling(context.Background(), pid, month)
	if err != nil {
		t.Fatalf("биллинг: %v", err)
	}
	return out.Totals.Views
}

// bumpViews — ролик продолжает набирать просмотры уже после фиксации.
func bumpViews(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, views int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE video_stat_daily SET views = $2
WHERE link_id IN (
    SELECT l.id FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    WHERE p.project_id = $1
)`, pid, views); err != nil {
		t.Fatalf("подменить просмотры: %v", err)
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

func snapshotRows(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM project_month_views WHERE project_id = $1`, pid).Scan(&n); err != nil {
		t.Fatalf("считать срез: %v", err)
	}
	return n
}

func firstOfThisMonth() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
