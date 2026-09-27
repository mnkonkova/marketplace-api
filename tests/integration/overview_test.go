package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/billing"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Сводка заказчика по всем проектам.
//
// Кросс-проектного среза в продукте не было: заказчик с тремя проектами
// складывал числа в уме. Главный вопрос экрана — «сколько мне стоит
// тысяча просмотров» — внутри одного проекта не считается вовсе.

// Сводка отдаёт свои проекты, все пять площадок и фактическую стоимость
// тысячи — и не отдаёт ничего из наших денег.
func TestClientOverviewShowsOwnProjectsOnly(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	// Два проекта одного заказчика с РАЗНЫМИ условиями: средняя ставка
	// по ним и фактическая стоимость тысячи — разные числа, и тест
	// падает, если считать по ставке.
	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()

	first, cleanupFirst := overviewProject(t, pool, client, 6_000_000, 9_000, 2_000_000, "ovw1")
	defer cleanupFirst()
	second, cleanupSecond := overviewProject(t, pool, client, 3_000_000, 3_000, 1_000_000, "ovw2")
	defer cleanupSecond()

	// Чужой проект другого заказчика — в сводку попасть не должен.
	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupStranger()
	_, cleanupForeign := overviewProject(t, pool, stranger, 6_000_000, 9_000, 5_000_000, "ovw3")
	defer cleanupForeign()

	// Тестовый проект того же заказчика — тоже не в счёт.
	testPID, cleanupTest := overviewProject(t, pool, client, 6_000_000, 9_000, 7_000_000, "ovw4")
	defer cleanupTest()
	if _, err := pool.Exec(ctx, `UPDATE projects SET is_test = TRUE WHERE id = $1`, testPID); err != nil {
		t.Fatalf("пометить тестовым: %v", err)
	}

	raw := rawBody(t, h, "/api/v1/me/overview", h.Token(t, client))

	// Наших денег в ответе нет ни на какой глубине.
	assertNoForbiddenKeys(t, raw, forbiddenForClient, allowedCreatorKeys)

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	if got := num(t, body, "projects_total"); got != 2 {
		t.Errorf("проектов в сводке %d, ожидали два своих не-тестовых", got)
	}
	seen := map[string]bool{}
	for _, raw := range list(t, body, "projects") {
		m, _ := raw.(map[string]any)
		id, _ := m["project_id"].(string)
		seen[id] = true
	}
	if !seen[first.String()] || !seen[second.String()] {
		t.Errorf("свои проекты не попали в сводку: %v", seen)
	}
	if seen[testPID.String()] {
		t.Error("тестовый проект попал в сводку")
	}

	// Все пять площадок всегда: пропавший столбик читается как сбой.
	byPlatform := subMap(t, subMap(t, body, "views"), "by_platform")
	for _, p := range []string{"tiktok", "instagram", "youtube", "vk", "likee"} {
		if _, ok := byPlatform[p]; !ok {
			t.Errorf("в разбивке по площадкам нет %q", p)
		}
	}
	views := num(t, subMap(t, body, "views"), "total")
	if views != 3_000_000 {
		t.Fatalf("просмотров всего %d, ожидали 3000000 (2М + 1М своих)", views)
	}

	// Стоимость тысячи — из фактических сумм, а не из средней ставки.
	money := subMap(t, body, "money")
	total := num(t, money, "total")
	if total <= 0 {
		t.Fatalf("счёт по своим проектам %d", total)
	}
	want := int64(total) * 1000 / int64(views)
	if got := int64(num(t, body, "cost_per_1000")); got != want {
		t.Errorf("стоимость тысячи %d, ожидали %d — она считается из фактических сумм", got, want)
	}
	// Средняя ставка по двум тарифам (9000 и 3000) дала бы 6000 —
	// убеждаемся, что тест вообще способен поймать такую ошибку.
	if want == 6000 {
		t.Fatal("подобрали данные так, что фактическая цена совпала со средней ставкой — тест ничего не проверяет")
	}
}

// Проект без вышедших роликов присутствует в сводке, но со своим
// состоянием, а не с нулями: «ещё не начался» и «работаем, но ничего не
// набрали» — разные новости.
func TestClientOverviewMarksNotStartedProjects(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	started, cleanupStarted := overviewProject(t, pool, client, 6_000_000, 9_000, 500_000, "ovw5")
	defer cleanupStarted()

	// Проект есть, роликов нет.
	idle, cleanupIdle := emptyClientProject(t, pool, client)
	defer cleanupIdle()

	_, body := h.Do(t, http.MethodGet, "/api/v1/me/overview", h.Token(t, client), nil)
	states := map[string]string{}
	periods := map[string]bool{}
	for _, raw := range list(t, body, "projects") {
		m, _ := raw.(map[string]any)
		id, _ := m["project_id"].(string)
		st, _ := m["state"].(string)
		states[id] = st
		_, hasPeriod := m["period"]
		periods[id] = hasPeriod
	}
	if states[idle.String()] != "not_started" {
		t.Errorf("проект без роликов помечен %q, ожидали not_started", states[idle.String()])
	}
	if periods[idle.String()] {
		t.Error("у не начавшегося проекта есть период — периодов до первой публикации не бывает")
	}
	if states[started.String()] != "running" {
		t.Errorf("проект с роликами помечен %q, ожидали running", states[started.String()])
	}
	if !periods[started.String()] {
		t.Error("у идущего проекта нет периода")
	}
}

// Подытоженный период в сводке не меняется: числа берутся из среза.
func TestClientOverviewUsesSnapshotForLockedPeriods(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 400_000, "ovw6")
	defer cleanup()

	// Ролик вышел два месяца назад — первый период давно кончился.
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET published_at = now() - interval '2 months',
                             submitted_at = now() - interval '2 months'
WHERE publication_id IN (SELECT id FROM project_publications WHERE project_id = $1)`,
		pid); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}
	// Ряд должен быть ДО отсечки: срез берёт последнее значение не позже
	// неё, а строка «на сегодня» для двухмесячной давности периода уже
	// после.
	resetDailyViews(t, pool, pid)
	seedDailyViews(t, pool, pid, time.Now().UTC().AddDate(0, -1, -3), 400_000)

	svc := billing.NewService(billing.NewRepo(pool))
	if _, _, err := svc.LockDuePeriods(ctx, time.Now().UTC(), billing.DefaultPeriodLockDelay); err != nil {
		t.Fatalf("подытог: %v", err)
	}

	_, before := h.Do(t, http.MethodGet, "/api/v1/me/overview", h.Token(t, client), nil)
	viewsBefore := num(t, subMap(t, before, "views"), "total")
	moneyBefore := num(t, subMap(t, before, "money"), "total")
	if viewsBefore != 400_000 {
		t.Fatalf("просмотров в сводке %d, ожидали 400000", viewsBefore)
	}

	// Ролик продолжает набирать просмотры уже после подытога.
	if _, err := pool.Exec(ctx, `
UPDATE video_stat_daily SET views = 9000000
WHERE link_id IN (
    SELECT l.id FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    WHERE p.project_id = $1
)`, pid); err != nil {
		t.Fatalf("подменить просмотры: %v", err)
	}

	_, after := h.Do(t, http.MethodGet, "/api/v1/me/overview", h.Token(t, client), nil)
	if got := num(t, subMap(t, after, "views"), "total"); got != viewsBefore {
		t.Errorf("просмотры подытоженного периода изменились: было %d, стало %d", viewsBefore, got)
	}
	if got := num(t, subMap(t, after, "money"), "total"); got != moneyBefore {
		t.Errorf("счёт подытоженного периода изменился: было %d, стало %d", moneyBefore, got)
	}
}

// ---- фикстуры ----

// overviewProject — проект заказчика с одним вышедшим роликом,
// собственными условиями и заданными просмотрами.
func overviewProject(t *testing.T, pool *pgxpool.Pool, client uuid.UUID,
	salary, rate int64, views int64, marker string) (uuid.UUID, func()) {

	t.Helper()
	ctx := context.Background()

	var pid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO projects (client_user_id, kind, title, source, status)
VALUES ($1, 'creators_turnkey', $2, 'manual', 'active')
RETURNING id`, client, "Сводка "+marker).Scan(&pid); err != nil {
		t.Fatalf("проект: %v", err)
	}
	creator, cleanupCreator := newAPIHarness(t, pool).NewUser(t, userOpts{Kind: "specialist"})

	if err := publications.NewRepo(pool).AddCreator(ctx, pid, creator, creator); err != nil {
		t.Fatalf("креатор в проект: %v", err)
	}

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, billing.Terms{
		ProjectID:            pid,
		SalaryPerMonth:       salary,
		RatePer1000Views:     rate,
		BonusViewsThreshold:  10_000_000,
		RatePer1000ViewsOver: rate / 10,
	}, creator); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creator, nextDue(), marker, views, true)

	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, pid.String())
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, pid)
		cleanupCreator()
	}
	return pid, cleanup
}

// emptyClientProject — проект заказчика, в котором ещё ничего не выходило.
func emptyClientProject(t *testing.T, pool *pgxpool.Pool, client uuid.UUID) (uuid.UUID, func()) {
	t.Helper()
	ctx := context.Background()
	var pid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO projects (client_user_id, kind, title, source, status)
VALUES ($1, 'creators_turnkey', 'Сводка: ещё не начался', 'manual', 'active')
RETURNING id`, client).Scan(&pid); err != nil {
		t.Fatalf("проект: %v", err)
	}
	return pid, func() { _, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, pid) }
}

// ---- тарифная лесенка ----
//
// Главный коммерческий аргумент кабинета: первые просмотры КАЖДОГО
// ролика идут по стартовой ставке, всё сверх — по пониженной, и отсюда
// берётся цена тысячи. До этого на экране стоял только результат
// формулы, а сама формула не приезжала ни в одном ответе.
//
// Лесенка обязана СХОДИТЬСЯ с числами, которые стоят рядом с ней: её
// проверяют калькулятором первым же заходом. Поэтому сервер отдаёт её
// только тогда, когда сумма ступеней равна счёту, а просмотры ступеней —
// тем просмотрам, из которых посчитана цена тысячи.

// ladderProject — проект на условиях стенда: 90 ₽ за тысячу до миллиона
// НА РОЛИК, 9 ₽ сверх, фикс 60 000 ₽ за период.
func ladderProject(t *testing.T, pool *pgxpool.Pool, client uuid.UUID,
	views int64, marker string) (uuid.UUID, func()) {

	t.Helper()
	ctx := context.Background()

	var pid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO projects (client_user_id, kind, title, source, status)
VALUES ($1, 'creators_turnkey', $2, 'manual', 'active')
RETURNING id`, client, "Лесенка "+marker).Scan(&pid); err != nil {
		t.Fatalf("проект: %v", err)
	}
	creator, cleanupCreator := newAPIHarness(t, pool).NewUser(t, userOpts{Kind: "specialist"})
	if err := publications.NewRepo(pool).AddCreator(ctx, pid, creator, creator); err != nil {
		t.Fatalf("креатор в проект: %v", err)
	}
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, billing.Terms{
		ProjectID:            pid,
		SalaryPerMonth:       60_000 * 100,
		RatePer1000Views:     90 * 100,
		BonusViewsThreshold:  1_000_000,
		RatePer1000ViewsOver: 9 * 100,
	}, creator); err != nil {
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, pid, creator, nextDue(), marker, views, true)

	return pid, func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, pid.String())
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, pid)
		cleanupCreator()
	}
}

func TestOverviewTariffLadderAddsUpToCostPer1000(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	// Три миллиона на один ролик: миллион по стартовой ставке, два — по
	// пониженной. Ровно случай со стенда.
	_, cleanup := ladderProject(t, pool, client, 3_000_000, "ldr1")
	defer cleanup()

	body := dashboardBody(t, h, client, "month")

	raw, ok := body["tariff"].(map[string]any)
	if !ok {
		t.Fatalf("лесенки нет в ответе, а разложение сходится: %v", body["cost_per_1000"])
	}
	// Контрольные точки коммерческого предложения — те самые числа,
	// которые стоят на экране заказчика.
	for key, want := range map[string]int{
		"threshold_views":    1_000_000,
		"rate_per_1000":      9_000,
		"rate_per_1000_over": 900,
		"views_base":         1_000_000,
		"views_over":         2_000_000,
		"base_amount":        90_000 * 100,
		"over_amount":        18_000 * 100,
		"fixed":              60_000 * 100,
		"total":              168_000 * 100,
		"views":              3_000_000,
	} {
		if got := num(t, raw, key); got != want {
			t.Errorf("%s = %d, ожидалось %d", key, got, want)
		}
	}
	// И главное: лесенка делится в ту цену тысячи, что стоит рядом.
	if got := num(t, body, "cost_per_1000"); got != 56*100 {
		t.Errorf("цена тысячи %d, ожидалось %d", got, 56*100)
	}
}

// Подытоженный период оставляет часть счёта ЗА лесенкой: его начислений
// уже нет, есть только сумма среза. Показать лесенку рядом с целым
// счётом значило бы поставить на экран деление, которое не сходится, —
// а его проверяют калькулятором.
func TestOverviewTariffHiddenWhenProjectsDisagree(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	_, cleanupA := ladderProject(t, pool, client, 3_000_000, "ldr2")
	defer cleanupA()
	// Второй проект на ДРУГИХ ставках: строка «первый миллион по 90 ₽»
	// была бы неправдой для половины счёта, а сложить две лесенки в одну
	// нельзя — читатель решит, что порог общий.
	_, cleanupB := overviewProject(t, pool, client, 3_000_000, 3_000, 1_000_000, "ldr3")
	defer cleanupB()

	body := dashboardBody(t, h, client, "month")
	if _, ok := body["tariff"]; ok {
		t.Errorf("лесенка есть, хотя у проектов разные ставки: %v", body["tariff"])
	}
}
