package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/billing"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Что уезжает по сети заказчику и креатору.
//
// Проверяем СЫРОЙ JSON ответа, а не структуры Go: именно байты уходят в
// браузер, и защита «фронт этого не рисует» — не защита. Заказчику
// хватало вкладки «Сеть», чтобы прочитать, сколько мы платим креатору и
// сколько оставляем себе.

// forbiddenForClient — ключи, которых в ответе заказчику быть не должно.
// payout_* — выплаты креатору, margin/payouts — маржа площадки,
// creator_*rate*/creator_salary — креаторская сторона тарифа.
var forbiddenForClient = []string{
	"payouts", "margin",
	"payout_salary", "payout_deduction", "payout_views_bonus",
	"payout_click_bonus", "payout_total",
	"creator_salary_per_month", "creator_rate_per_1000_views",
	"creator_rate_per_1000_views_over",
	// Перенос остатка ступени креаторской стороны — внутренняя механика
	// выплат. Его тут не было, и дыру в периоде поэтому никто не поймал:
	// список запретных ключей должен расти вместе с полями про деньги.
	"carry_in_creator", "carry_out_creator",
	// Идентификаторы периода и цепочка переноса — наша механика.
	"prev_period_id",
	// UTM — рабочий инструмент менеджера, заказчику не отдаётся.
	"utm",
}

// forbiddenForCreator — то же для креатора: клиентская сторона денег и
// наша механика.
var forbiddenForCreator = []string{
	"carry_in_client", "carry_out_client",
	"creator_salary_per_month", "creator_rate_per_1000_views",
	"creator_rate_per_1000_views_over",
	"payout_salary", "payout_deduction", "payout_views_bonus",
	"payout_click_bonus", "payout_total",
	"margin", "payouts", "prev_period_id",
}

// allowedCreatorKeys — ключи со словом creator, которые заказчику
// положены: они говорят, ЧЬЯ строка, а не сколько человек получит.
var allowedCreatorKeys = map[string]bool{
	"creator_user_id": true,
	"creator_name":    true,
}

// Ответ заказчику не содержит наших денег.
func TestClientBillingHidesOurMoney(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	pid, clientID, _, cleanup := setupBillingWithMargin(t, pool)
	defer cleanup()

	raw := rawBody(t, h, "/api/v1/me/projects/"+pid.String()+"/billing", h.Token(t, clientID))

	assertNoForbiddenKeys(t, raw, forbiddenForClient, allowedCreatorKeys)

	// И то же самое подстрокой: если кто-то переименует поле в структуре,
	// обход по ключам его не узнает, а текст ответа всё равно не должен
	// содержать этих слов рядом с двоеточием.
	for _, key := range []string{`"payouts":`, `"margin":`, `"payout_total":`, `"creator_salary_per_month":`} {
		if strings.Contains(raw, key) {
			t.Errorf("в ответе заказчику встретился %s", key)
		}
	}

	// Период в ответе — его стороной: границы и его перенос есть,
	// креаторского и наших идентификаторов нет (их отсутствие уже
	// проверено выше по списку запретных ключей).
	var periodBody map[string]any
	if err := json.Unmarshal([]byte(raw), &periodBody); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	period := subMap(t, periodBody, "period")
	for _, key := range []string{"seq", "starts_on", "ends_on", "status", "carry_in_client", "carry_out_client"} {
		if _, ok := period[key]; !ok {
			t.Errorf("в периоде заказчика нет %q", key)
		}
	}
	if _, ok := period["id"]; ok {
		t.Error("в периоде заказчика есть id — это наша механика, не его")
	}

	// Чистка не должна была выкинуть лишнего: заказчик по-прежнему
	// видит свою сторону тарифа, свои начисления и итоги месяца.
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	terms := subMap(t, body, "terms")
	if got := num(t, terms, "salary_per_month"); got != 6_000_000 {
		t.Errorf("оклад в тарифе заказчика %d, ожидали цену клиента 6000000", got)
	}
	for _, key := range []string{"rate_per_1000_views", "bonus_views_threshold", "rate_per_1000_views_over"} {
		if _, ok := terms[key]; !ok {
			t.Errorf("в тарифе заказчика нет %q — чистка выкинула лишнее", key)
		}
	}
	accruals := list(t, body, "accruals")
	if len(accruals) == 0 {
		t.Fatal("заказчику не показали команду месяца")
	}
	row, _ := accruals[0].(map[string]any)
	for _, key := range []string{
		"creator_user_id", "salary", "deduction", "views_bonus", "total",
		"videos_planned", "videos_delivered", "views_total",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("в строке начисления нет %q — чистка выкинула лишнее", key)
		}
	}
	totals := subMap(t, body, "totals")
	for _, key := range []string{"salaries", "deductions", "views_bonus", "total", "views", "cost_per_1000"} {
		if _, ok := totals[key]; !ok {
			t.Errorf("в итогах месяца нет %q — чистка выкинула лишнее", key)
		}
	}
	if _, ok := body["payments"]; !ok {
		t.Error("в ответе нет платежей заказчика")
	}

	// Счёт заказчику — это цена клиента, а не выплата креатору: если бы
	// в клиентский ответ поехали payout-числа, итог совпал бы с ними.
	svc := billing.NewService(billing.NewRepo(pool))
	full, err := svc.ProjectBilling(context.Background(), pid, 0, time.Now().UTC())
	if err != nil {
		t.Fatalf("менеджерский биллинг: %v", err)
	}
	if full.Totals.Payouts == full.Totals.Total {
		t.Fatal("в наборе данных выплата совпала со счётом — тест ничего не проверяет")
	}
	if got := int64(num(t, totals, "total")); got != full.Totals.Total {
		t.Errorf("итог заказчику %d, ожидали счёт клиента %d", got, full.Totals.Total)
	}
}

// Ответ креатору не содержит цены клиента.
func TestCreatorEarningsHideClientPrice(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	pid, _, creators, cleanup := setupBillingWithMargin(t, pool)
	defer cleanup()

	raw := rawBody(t, h,
		"/api/v1/me/creator/projects/"+pid.String()+"/earnings", h.Token(t, creators[0]))

	// Креаторская сторона тарифа креатору отдаётся под обычными именами
	// (salary_per_month), поэтому запрещены здесь именно creator_*-поля:
	// это «вторая сторона», по которой считается наша маржа.
	assertNoForbiddenKeys(t, raw, []string{
		"creator_salary_per_month", "creator_rate_per_1000_views",
		"creator_rate_per_1000_views_over",
		"payout_salary", "payout_deduction", "payout_views_bonus",
		"payout_click_bonus", "payout_total",
		"margin", "payouts",
	}, allowedCreatorKeys)

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	terms := subMap(t, body, "terms")
	if got := num(t, terms, "salary_per_month"); got != 4_500_000 {
		t.Errorf("оклад в ответе креатора %d, ожидали его ставку 4500000 "+
			"(цена клиента — 6000000)", got)
	}
	accruals := list(t, body, "accruals")
	if len(accruals) == 0 {
		t.Fatal("креатору не показали его начисления")
	}
	row, _ := accruals[0].(map[string]any)
	for _, key := range []string{"salary", "deduction", "views_bonus", "total", "views_total"} {
		if _, ok := row[key]; !ok {
			t.Errorf("в строке заработка нет %q — чистка выкинула лишнее", key)
		}
	}
	// Чужих строк в ответе нет.
	if len(accruals) != 1 {
		t.Errorf("креатору видно %d начислений, ожидали только своё", len(accruals))
	}
	if got := row["creator_user_id"]; got != creators[0].String() {
		t.Errorf("в ответе чужая строка: %v", got)
	}
}

// Период в ответе креатора — его стороной: границы и его перенос есть,
// клиентского переноса нет.
//
// Без периода фронт выводил границы сам, прибавляя месяц к дате начала:
// второе описание правила периода, которое разъедется с сервером на
// первой же правке. А клиентский перенос — та же коммерческая тайна,
// что и цена клиента.
func TestCreatorEarningsCarryPeriodWithoutClientSide(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	pid, _, creators, cleanup := setupBillingWithMargin(t, pool)
	defer cleanup()

	// Ролик вышел — значит у проекта есть период.
	if _, err := pool.Exec(context.Background(), `
UPDATE publication_links SET published_at = now() - interval '3 days'
WHERE publication_id IN (SELECT id FROM project_publications WHERE project_id = $1)`,
		pid); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}

	raw := rawBody(t, h,
		"/api/v1/me/creator/projects/"+pid.String()+"/earnings", h.Token(t, creators[0]))

	// Клиентской стороны переноса в ответе нет ни на какой глубине.
	assertNoForbiddenKeys(t, raw, forbiddenForCreator, allowedCreatorKeys)

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	period := subMap(t, body, "period")
	for _, key := range []string{
		"seq", "starts_on", "ends_on", "status", "carry_in_creator", "carry_out_creator",
	} {
		if _, ok := period[key]; !ok {
			t.Errorf("в периоде креатора нет %q — фронту снова считать границы самому", key)
		}
	}
	if got := num(t, period, "seq"); got != 1 {
		t.Errorf("номер периода %d, ожидали первый", got)
	}
	if got, _ := period["status"].(string); got != "open" {
		t.Errorf("состояние периода %q, ожидали open", got)
	}
	// Перенос пока нулевой, но поле есть: арифметику включат с тарифом.
	if got := num(t, period, "carry_in_creator"); got != 0 {
		t.Errorf("перенос на входе %d, ожидали ноль до появления тарифа", got)
	}
	// Периоды списком — по ним же приходят строки начислений.
	if items := list(t, body, "periods"); len(items) == 0 {
		t.Error("список периодов пуст — фронту нечем подписать прошлые строки")
	}
}

// Список проектов специалиста не отдаёт внутренние заметки менеджера.
//
// Карточка проекта их уже не отдавала (data-sec D4), а список —
// отдавал: та же структура, собранная для менеджера, просто на другом
// экране.
func TestSpecialistListHidesManagerNotes(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	_, _, projectID, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	specialist, cleanupSpec := h.NewUser(t, userOpts{Kind: "specialist"})
	defer cleanupSpec()

	const secret = "внутренняя заметка: клиент торгуется, скидку не давать"
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET specialist_user_id = $2, notes = $3 WHERE id = $1`,
		projectID, specialist, secret); err != nil {
		t.Fatalf("подготовить проект: %v", err)
	}

	raw := rawBody(t, h, "/api/v1/me/specialist/projects", h.Token(t, specialist))
	if strings.Contains(raw, secret) {
		t.Errorf("специалисту уехали внутренние заметки менеджера:\n%s", raw)
	}
	if strings.Contains(raw, `"notes"`) {
		t.Errorf("в списке специалиста есть поле notes:\n%s", raw)
	}
	// Проект при этом виден — чистка не должна была выкинуть его целиком.
	if !strings.Contains(raw, projectID.String()) {
		t.Errorf("проект пропал из списка специалиста:\n%s", raw)
	}
}

// ---- помощники ----

// rawBody — тело ответа как есть. Do разбирает JSON в map и для проверки
// «что именно уехало по сети» не годится.
func rawBody(t *testing.T, h *integration.APIHarness, path, token string) string {
	t.Helper()
	rec := h.DoRaw(t, http.MethodGet, path, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: код %d, тело %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// assertNoForbiddenKeys — обходит весь JSON и проверяет каждый ключ на
// любой глубине. Проверка по структуре Go поймала бы не то: по сети
// уходят байты, а не структура.
func assertNoForbiddenKeys(t *testing.T, raw string, forbidden []string, allowed map[string]bool) {
	t.Helper()
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	banned := map[string]bool{}
	for _, k := range forbidden {
		banned[k] = true
	}
	found := map[string]bool{}
	walkJSONKeys(parsed, func(key string) {
		if allowed[key] {
			return
		}
		if banned[key] || strings.HasPrefix(key, "payout_") {
			found[key] = true
		}
	})
	if len(found) > 0 {
		keys := make([]string, 0, len(found))
		for k := range found {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("в ответе есть поля, которых там быть не должно: %s\nтело: %s",
			strings.Join(keys, ", "), raw)
	}
}

func walkJSONKeys(v any, visit func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			visit(k)
			walkJSONKeys(sub, visit)
		}
	case []any:
		for _, sub := range t {
			walkJSONKeys(sub, visit)
		}
	}
}

// setupBillingWithMargin — проект с посчитанным месяцем, где выплата
// креатору заведомо меньше счёта заказчику. Без этой разницы тест на
// утечку прошёл бы и на данных, где все числа совпадают.
func setupBillingWithMargin(t *testing.T, pool *pgxpool.Pool) (projectID, clientID uuid.UUID, creators []uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	projectID, creators, cleanup = setupCreatorsProject(t, pool)

	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, projectID).Scan(&clientID); err != nil {
		cleanup()
		t.Fatalf("клиент проекта: %v", err)
	}

	svc := billing.NewService(billing.NewRepo(pool))
	terms := demoTerms(projectID)
	creatorSalary, creatorRate, creatorOver := int64(4_500_000), int64(7_000), int64(700)
	terms.CreatorSalaryPerMonth = &creatorSalary
	terms.CreatorRatePer1000Views = &creatorRate
	terms.CreatorRatePer1000ViewsOver = &creatorOver
	if _, err := svc.SaveTerms(ctx, terms, creators[0]); err != nil {
		cleanup()
		t.Fatalf("условия: %v", err)
	}
	seedPublicationViews(t, projectID, creators[0], pubDay(0),
		fmt.Sprintf("priv%s", uuid.NewString()[:6]), 1_200_000, true)
	if _, err := recalcCurrent(t, svc, projectID); err != nil {
		cleanup()
		t.Fatalf("пересчёт: %v", err)
	}
	return projectID, clientID, creators, cleanup
}

// Обезличенный ориентир проекта в кабинете креатора: ниже порога его
// нет, выше — есть и считается по зрелым роликам.
//
// Порог здесь не оптимизация, а граница: в проекте с двумя креаторами
// «медиана проекта» — это показатель соседа, и отдать её значит своими
// руками показать одному креатору результаты другого.
func TestCreatorBenchmarkHiddenBelowAnonymityThreshold(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	// Двое креаторов и три зрелых ролика — порог не пройден.
	old := time.Now().UTC().AddDate(0, 0, -30)
	publishOn(t, pool, pid, creators[0], old, "bmk01", 100_000)
	publishOn(t, pool, pid, creators[1], old, "bmk02", 200_000)
	publishOn(t, pool, pid, creators[0], old, "bmk03", 300_000)

	raw := rawBody(t, h,
		"/api/v1/me/creator/projects/"+pid.String()+"/earnings", h.Token(t, creators[0]))
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	if _, ok := body["benchmark"]; ok {
		t.Fatalf("ниже порога обезличивания ориентир отдавать нельзя: %v", body["benchmark"])
	}

	// Добавляем третьего креатора и добиваем до десяти зрелых роликов.
	third, cleanupThird := h.NewUser(t, userOpts{Kind: "specialist"})
	defer cleanupThird()
	if err := publicationsRepoAddCreator(t, pool, pid, third); err != nil {
		t.Fatalf("третий креатор: %v", err)
	}
	views := []int64{400_000, 500_000, 600_000, 700_000, 800_000, 900_000, 1_000_000}
	for i, v := range views {
		author := creators[i%2]
		if i%3 == 0 {
			author = third
		}
		publishOn(t, pool, pid, author, old, fmt.Sprintf("bmk1%d", i), v)
	}
	// Ролик моложе двух недель в медиану входить не должен.
	publishOn(t, pool, pid, creators[0], time.Now().UTC().AddDate(0, 0, -2), "bmkYoung", 50_000_000)

	raw = rawBody(t, h,
		"/api/v1/me/creator/projects/"+pid.String()+"/earnings", h.Token(t, creators[0]))
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	bench := subMap(t, body, "benchmark")
	mature := num(t, bench, "mature_videos")
	if mature != 10 {
		t.Fatalf("зрелых роликов в расчёте %d, ожидали 10 (свежий не в счёт)", mature)
	}
	median := int64(num(t, bench, "project_median_views"))
	if median != medianOfMature(t, pool, pid) {
		t.Errorf("медиана %d не совпала с медианой зрелых роликов проекта", median)
	}
	// Свежий ролик на 50 млн не утащил медиану вверх.
	if median > 1_000_000 {
		t.Errorf("медиана %d — в неё попал ролик моложе двух недель", median)
	}

	// В блоке нет ничьих идентификаторов и ничьих отдельных чисел.
	for key := range bench {
		switch key {
		case "project_median_views", "my_percentile", "mature_videos":
		default:
			t.Errorf("в обезличенном блоке лишнее поле %q", key)
		}
	}
	// И весь ответ по-прежнему без клиентской стороны денег.
	assertNoForbiddenKeys(t, raw, forbiddenForCreator, allowedCreatorKeys)
}

// publicationsRepoAddCreator — добавить креатора в проект.
func publicationsRepoAddCreator(t *testing.T, pool *pgxpool.Pool, pid, creator uuid.UUID) error {
	t.Helper()
	return publications.NewRepo(pool).AddCreator(context.Background(), pid, creator, creator)
}

// medianOfMature — медиана просмотров зрелых роликов проекта, считанная
// независимо от кода приложения.
func medianOfMature(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) int64 {
	t.Helper()
	var median int64
	if err := pool.QueryRow(context.Background(), `
WITH mature AS (
    SELECT p.id, COALESCE(SUM(cur.views), 0) AS views
    FROM project_publications p
    JOIN publication_links l ON l.publication_id = p.id
    LEFT JOIN LATERAL (
        SELECT views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE p.project_id = $1 AND p.status <> 'cancelled'
    GROUP BY p.id
    HAVING MIN(COALESCE(l.published_at, l.submitted_at)) <= now() - interval '14 days'
)
SELECT COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY views), 0)::bigint FROM mature`,
		pid).Scan(&median); err != nil {
		t.Fatalf("медиана: %v", err)
	}
	return median
}
