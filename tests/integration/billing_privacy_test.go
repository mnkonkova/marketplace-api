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
	// UTM — рабочий инструмент менеджера, заказчику не отдаётся.
	"utm",
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
	full, err := svc.ProjectBilling(context.Background(), pid, time.Now().UTC())
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
	if _, err := svc.Recalculate(ctx, projectID, time.Now().UTC()); err != nil {
		cleanup()
		t.Fatalf("пересчёт: %v", err)
	}
	return projectID, clientID, creators, cleanup
}
