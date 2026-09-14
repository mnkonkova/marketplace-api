package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/tests/integration"
)

// Package e2e — сквозные сценарии: путь пользователя целиком, а не
// отдельная ручка.
//
// Лежат отдельно от tests/integration намеренно. Там проверяют по одному
// свойству за раз и не стесняются подготовить состояние напрямую в базе;
// здесь коротких путей нет вовсе — только HTTP через настоящий роутер, с
// настоящими токенами и проверкой ролей. Ровно поэтому эти тесты и
// находят то, чего не видят остальные: сервисные тесты чинили базу за
// приложение и не замечали, что приложение так не умеет.
//
// Сквозной сценарий: месяц работы с креаторами, от согласия с условиями
// до выплаты.
//
// Отличие от остальных тестов в том, что здесь нет коротких путей. Никаких
// «положим проекту kind правкой базы» и «вызовем сервис напрямую» — только
// HTTP через настоящий роутер, с настоящими токенами и настоящей проверкой
// ролей. Именно так этот тест и нашёл, что вид проекта не выставляла ни
// одна ручка: сервисные тесты этого не видели, потому что чинили базу за
// приложение.
//
// Что не сквозное и почему: статистику просмотров кладём в базу руками.
// Её собирает instacurl, ходящий в TikTok и VK; поднимать пять внешних
// площадок ради теста бессмысленно, а всё, что идёт ПОСЛЕ сбора — бонусы,
// пороги, выплаты — проверяется полностью.

// e2eWorld — участники сценария.
type e2eWorld struct {
	h        *integration.APIHarness
	client   uuid.UUID
	creator  uuid.UUID
	manager  uuid.UUID
	clientTk string
	crTk     string
	mgrTk    string
	termsID  uuid.UUID
}

func setupE2E(t *testing.T) *e2eWorld {
	t.Helper()
	pool := integration.Pool(t)
	h := integration.NewAPIHarness(t, pool)
	ctx := context.Background()

	client, cleanupClient := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanupClient)
	creator, cleanupCreator := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanupCreator)
	manager, cleanupManager := h.NewUser(t, integration.UserOpts{Kind: "client", IsManager: true})
	t.Cleanup(cleanupManager)

	// Креатор должен быть опубликованным специалистом из креаторской
	// категории — иначе подбор его не пропустит, и это правильно.
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, is_published, social_links)
VALUES ($1, 'Анастасия Креатор', TRUE, '{"tiktok":"https://tiktok.com/@nastya"}'::jsonb)`,
		creator); err != nil {
		t.Fatalf("профиль креатора: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_categories (user_id, category_code, is_primary)
VALUES ($1, 'ugc', TRUE) ON CONFLICT DO NOTHING`, creator); err != nil {
		t.Fatalf("категория креатора: %v", err)
	}

	// Действующая версия правил с тарифом из референса: оклад 60 000 ₽,
	// 90 ₽ за 1000 просмотров до миллиона на ролик и 9 ₽ свыше.
	var termsID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO terms_versions (version, body, salary_per_month, videos_first_month,
                            videos_next_months, rate_per_1000_views,
                            bonus_views_threshold, rate_per_1000_views_over)
VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions),
        'Условия работы', 6000000, 30, 60, 9000, 1000000, 900)
RETURNING id`).Scan(&termsID); err != nil {
		t.Fatalf("версия правил: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, termsID) })

	return &e2eWorld{
		h: h, client: client, creator: creator, manager: manager,
		clientTk: h.Token(t, client), crTk: h.Token(t, creator), mgrTk: h.Token(t, manager),
		termsID: termsID,
	}
}

// step — запрос с проверкой кода. Тело возвращается для следующего шага.
func (w *e2eWorld) step(t *testing.T, name, method, path, token string, body any, want int) map[string]any {
	t.Helper()
	code, out := w.h.Do(t, method, path, token, body)
	if code != want {
		t.Fatalf("%s: код %d, ожидался %d. тело: %v", name, code, want, out)
	}
	return out
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func num(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// ---- ТЕСТ: подбор креаторов и оплата ----

func TestE2EOrderToPaid(t *testing.T) {
	w := setupE2E(t)
	pool := integration.Pool(t)
	ctx := context.Background()

	// 1. Клиент читает условия и соглашается. Без согласия заказ не
	// создаётся вовсе — проверяем это же ниже.
	resp := w.step(t, "условия", http.MethodGet, "/api/v1/me/orders/terms", w.clientTk, nil, http.StatusOK)
	terms, _ := resp["terms"].(map[string]any)
	if num(terms, "salary_per_month") != 6000000 || num(terms, "rate_per_1000_views") != 9000 {
		t.Errorf("тариф не доехал до клиента: %v", resp)
	}
	if num(terms, "videos_first_month") != 30 {
		t.Errorf("объём, за который назван оклад: %v", terms["videos_first_month"])
	}
	if resp["consented"] != false {
		t.Errorf("до согласия признак должен быть false: %v", resp["consented"])
	}

	// 2. Сколько креаторов можно взять. Первый месяц — один.
	month := time.Now().UTC().AddDate(0, 1, 0).Format("2006-01")
	limit := w.step(t, "лимит", http.MethodGet,
		"/api/v1/me/orders/limit?month="+month, w.clientTk, nil, http.StatusOK)
	if num(limit, "allowed") != 1 {
		t.Errorf("в первый месяц доступен один креатор, получено %v", limit["allowed"])
	}
	if limit["first_month"] != true {
		t.Errorf("месяц должен быть первым: %v", limit)
	}

	// 3. Заказ без согласия не проходит.
	order := map[string]any{
		"start_month": month, "needed": 1, "videos_count": 30,
		"creator_ids": []string{w.creator.String()},
	}
	w.step(t, "заказ без согласия", http.MethodPost, "/api/v1/me/orders",
		w.clientTk, order, http.StatusForbidden)

	w.step(t, "согласие", http.MethodPost, "/api/v1/me/orders/terms/consent",
		w.clientTk, nil, http.StatusOK)

	// 4. Теперь заказ создаётся.
	created := w.step(t, "заказ", http.MethodPost, "/api/v1/me/orders",
		w.clientTk, order, http.StatusCreated)
	orderBody, _ := created["order"].(map[string]any)
	if orderBody == nil {
		orderBody = created
	}
	orderID := str(orderBody, "id")
	if orderID == "" {
		t.Fatalf("в ответе нет id заказа: %v", created)
	}

	// 5. Смета: оклад известен точно, прогноза нет — истории у креатора
	// ещё не было, и это должно быть видно, а не показано нулём.
	est := w.step(t, "смета", http.MethodGet,
		"/api/v1/me/orders/"+orderID+"/estimate", w.clientTk, nil, http.StatusOK)
	if num(est, "salaries") != 6000000 {
		t.Errorf("оклад в смете: %v", est["salaries"])
	}
	if est["has_forecast"] != false {
		t.Errorf("прогноз не может быть построен на пустой истории: %v", est)
	}
	if num(est, "without_history") != 1 {
		t.Errorf("без истории должен быть один: %v", est["without_history"])
	}

	// 6. Приглашения уходят по приоритету.
	w.step(t, "пригласить", http.MethodPost,
		"/api/v1/me/orders/"+orderID+"/invite", w.clientTk, nil, http.StatusOK)

	invites := w.step(t, "приглашения креатора", http.MethodGet,
		"/api/v1/me/creator/invitations", w.crTk, nil, http.StatusOK)
	items, _ := invites["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("креатор должен видеть одно приглашение: %v", invites)
	}

	// 7. Креатор соглашается — заказ собран.
	staffed := w.step(t, "согласие креатора", http.MethodPost,
		"/api/v1/me/creator/invitations/"+orderID+"/respond", w.crTk,
		map[string]any{"accept": true}, http.StatusOK)
	if str(staffed, "status") != "staffed" {
		t.Errorf("после согласия заказ должен быть собран: %v", staffed["status"])
	}

	// 8. Чужой креатор в этот заказ ответить не может.
	other, cleanupOther := w.h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	defer cleanupOther()
	w.step(t, "ответ постороннего", http.MethodPost,
		"/api/v1/me/creator/invitations/"+orderID+"/respond", w.h.Token(t, other),
		map[string]any{"accept": true}, http.StatusConflict)

	// 9. Клиент оплату не подтверждает — это делает менеджер.
	w.step(t, "оплата клиентом", http.MethodPost,
		"/api/v1/manager/orders/"+orderID+"/paid", w.clientTk, nil, http.StatusForbidden)

	paid := w.step(t, "оплата менеджером", http.MethodPost,
		"/api/v1/manager/orders/"+orderID+"/paid", w.mgrTk, nil, http.StatusOK)
	if str(paid, "status") != "paid" {
		t.Errorf("заказ должен быть оплачен: %v", paid["status"])
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM creator_orders WHERE id = $1`, orderID)
	})
}

// ---- ТЕСТ: месяц работы, от дат до выплаты ----

func TestE2EMonthOfWork(t *testing.T) {
	w := setupE2E(t)
	pool := integration.Pool(t)
	ctx := context.Background()

	// 1. Менеджер заводит проект с креаторами. Воронки у него нет — и до
	// этого теста такой проект через API вообще не создавался.
	project := w.step(t, "создать проект", http.MethodPost, "/api/v1/manager/projects", w.mgrTk,
		map[string]any{
			"kind": "creators_turnkey", "title": "PetFlat · UGC",
			"client_user_id": w.client.String(), "notes": "Вертикальные ролики про корм",
		}, http.StatusCreated)
	projectID := str(project, "id")
	if projectID == "" {
		t.Fatalf("проект не создан: %v", project)
	}
	if str(project, "kind") != "creators_turnkey" {
		t.Fatalf("вид проекта не сохранился: %v", project["kind"])
	}
	if project["pipeline_id"] != nil {
		t.Errorf("у проекта с креаторами не должно быть воронки: %v", project["pipeline_id"])
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate='project' AND aggregate_id=$1`, projectID)
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID)
	})

	// 2. Состав проекта.
	w.step(t, "добавить креатора", http.MethodPost,
		"/api/v1/manager/projects/"+projectID+"/creators", w.mgrTk,
		map[string]any{"creator_user_id": w.creator.String()}, http.StatusNoContent)

	roster := w.step(t, "состав", http.MethodGet,
		"/api/v1/manager/projects/"+projectID+"/creators", w.mgrTk, nil, http.StatusOK)
	people, _ := roster["items"].([]any)
	if len(people) != 1 {
		t.Fatalf("в составе должен быть один: %v", roster)
	}
	first, _ := people[0].(map[string]any)
	if str(first, "display_name") != "Анастасия Креатор" {
		t.Errorf("имя вместо uuid: %v", first["display_name"])
	}
	links, _ := first["account_links"].(map[string]any)
	if links["tiktok"] == nil {
		t.Errorf("ссылки на аккаунты не доехали: %v", first["account_links"])
	}

	// 3. Тариф проекта — снимком с действующего прайса.
	tariff := w.step(t, "тариф", http.MethodPost,
		"/api/v1/manager/projects/"+projectID+"/billing/adopt", w.mgrTk, nil, http.StatusOK)
	if num(tariff, "rate_per_1000_views") != 9000 || num(tariff, "bonus_views_threshold") != 1000000 {
		t.Fatalf("тариф не снялся: %v", tariff)
	}

	// 4. Даты выкладок пачкой. Две штуки: одна станет сданной, вторая нет.
	today := time.Now().UTC()
	batch := w.step(t, "пачка дат", http.MethodPost,
		"/api/v1/manager/projects/"+projectID+"/publications/batch", w.mgrTk,
		map[string]any{
			"creator_user_ids": []string{w.creator.String()},
			"dates": []string{
				today.Format("2006-01-02"),
				today.AddDate(0, 0, 1).Format("2006-01-02"),
			},
		}, http.StatusCreated)
	if num(batch, "created") != 2 {
		t.Fatalf("должно создаться две выкладки: %v", batch)
	}

	// 5. Креатор видит проект в своём списке и открывает карточку.
	mine := w.step(t, "мои проекты", http.MethodGet,
		"/api/v1/me/creator/projects", w.crTk, nil, http.StatusOK)
	list, _ := mine["items"].([]any)
	if len(list) != 1 {
		t.Fatalf("креатор должен видеть один проект: %v", mine)
	}
	card := w.step(t, "карточка проекта", http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID, w.crTk, nil, http.StatusOK)
	if str(card, "brief") != "Вертикальные ролики про корм" {
		t.Errorf("бриф не доехал: %v", card["brief"])
	}
	if num(card, "publications_total") != 2 {
		t.Errorf("счётчик выкладок: %v", card["publications_total"])
	}

	// 6. Креатор сдаёт ролик — все пять площадок.
	pubs := w.step(t, "выкладки креатора", http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID+"/publications", w.crTk, nil, http.StatusOK)
	pubItems, _ := pubs["items"].([]any)
	if len(pubItems) != 2 {
		t.Fatalf("креатор должен видеть свои две выкладки: %v", pubs)
	}
	firstPub, _ := pubItems[0].(map[string]any)
	pubID := str(firstPub, "id")

	done := w.step(t, "сдать ссылки", http.MethodPost,
		"/api/v1/me/creator/publications/"+pubID+"/links", w.crTk,
		map[string]any{"urls": []string{
			"https://www.tiktok.com/@nastya/video/7412093000",
			"https://www.instagram.com/reel/C9xK2mLpQ7v/",
			"https://www.youtube.com/shorts/kQ2Vn8pLxJc",
			"https://vk.com/clip-2394821_45623",
			"https://likee.video/@nastya/video/7412093000",
		}}, http.StatusOK)
	if str(done, "status") != "done" {
		t.Fatalf("пять площадок закрывают выкладку: %v", done["status"])
	}

	// 7. Чужой креатор в эту выкладку сдать не может.
	other, cleanupOther := w.h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	defer cleanupOther()
	w.step(t, "чужая сдача", http.MethodPost,
		"/api/v1/me/creator/publications/"+pubID+"/links", w.h.Token(t, other),
		map[string]any{"urls": []string{"https://www.tiktok.com/@x/video/1"}},
		http.StatusNotFound)

	// 8. Статистика. Единственный не сквозной шаг: её собирает instacurl,
	// ходящий на пять внешних площадок. Кладём в базу то, что он положил
	// бы, — 2 000 000 просмотров на ролик, то есть миллион по полной
	// ставке и миллион по пониженной.
	if _, err := pool.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, collected_at)
SELECT l.id, CURRENT_DATE,
       CASE WHEN l.platform = 'tiktok' THEN 2000000 ELSE 0 END, 1000, 100, now()
FROM publication_links l WHERE l.publication_id = $1`, pubID); err != nil {
		t.Fatalf("статистика: %v", err)
	}

	// 9. Клиент видит вышедший ролик и отчёт.
	videos := w.step(t, "ролики клиенту", http.MethodGet,
		"/api/v1/me/projects/"+projectID+"/videos", w.clientTk, nil, http.StatusOK)
	vItems, _ := videos["items"].([]any)
	if len(vItems) != 1 {
		t.Fatalf("клиент должен видеть один вышедший ролик: %v", videos)
	}
	video, _ := vItems[0].(map[string]any)
	if str(video, "creator_name") != "Анастасия Креатор" {
		t.Errorf("в ленте клиента имя, а не uuid: %v", video["creator_name"])
	}
	if num(video, "views") != 2000000 {
		t.Errorf("просмотры в ленте: %v", video["views"])
	}

	report := w.step(t, "отчёт", http.MethodGet,
		"/api/v1/me/projects/"+projectID+"/report", w.clientTk, nil, http.StatusOK)
	if num(report, "views") != 2000000 {
		t.Errorf("просмотры в отчёте: %v", report["views"])
	}

	// 10. Менеджер считает период.
	recalc := w.step(t, "пересчёт", http.MethodPost,
		"/api/v1/manager/projects/"+projectID+"/accruals/recalc", w.mgrTk, nil, http.StatusOK)
	accs, _ := recalc["items"].([]any)
	if len(accs) != 1 {
		t.Fatalf("должно быть одно начисление: %v", recalc)
	}
	acc, _ := accs[0].(map[string]any)
	accID := str(acc, "id")

	// Сдана одна выкладка из двух: половина оклада вычитается.
	if num(acc, "videos_planned") != 2 || num(acc, "videos_delivered") != 1 {
		t.Errorf("выкладки в начислении: %v", acc)
	}
	if num(acc, "deduction") != 3000000 {
		t.Errorf("недосданное не оплачивается — ждали половину оклада, получили %v", acc["deduction"])
	}
	// Ступени: миллион по 90 ₽ и миллион по 9 ₽.
	wantBonus := float64(1000000/1000*9000 + 1000000/1000*900)
	if num(acc, "views_bonus") != wantBonus {
		t.Errorf("бонус: ждали %v, получили %v (base=%v over=%v)",
			wantBonus, acc["views_bonus"], acc["views_base"], acc["views_over"])
	}

	// 11. Кнопки строго по очереди: выплатить неутверждённое нельзя.
	w.step(t, "выплата до утверждения", http.MethodPost,
		fmt.Sprintf("/api/v1/manager/projects/%s/accruals/%s/paid", projectID, accID),
		w.mgrTk, nil, http.StatusConflict)
	w.step(t, "утвердить", http.MethodPost,
		fmt.Sprintf("/api/v1/manager/projects/%s/accruals/%s/approve", projectID, accID),
		w.mgrTk, nil, http.StatusOK)
	final := w.step(t, "выплатить", http.MethodPost,
		fmt.Sprintf("/api/v1/manager/projects/%s/accruals/%s/paid", projectID, accID),
		w.mgrTk, nil, http.StatusOK)
	if str(final, "status") != "paid" {
		t.Errorf("после выплаты: %v", final["status"])
	}

	// 12. Клиент видит «Команду месяца» и счёт.
	bill := w.step(t, "счёт клиенту", http.MethodGet,
		"/api/v1/me/projects/"+projectID+"/billing", w.clientTk, nil, http.StatusOK)
	team, _ := bill["accruals"].([]any)
	if len(team) != 1 {
		t.Fatalf("клиент должен видеть команду месяца: %v", bill)
	}
	member, _ := team[0].(map[string]any)
	if str(member, "creator_name") != "Анастасия Креатор" {
		t.Errorf("в команде месяца имя: %v", member["creator_name"])
	}
	totals, _ := bill["totals"].(map[string]any)
	if num(totals, "total") != num(acc, "total") {
		t.Errorf("итог клиента разошёлся с начислением: %v против %v", totals["total"], acc["total"])
	}
	if bill["utm"] != nil {
		t.Errorf("заказчику не показываем UTM-метки: %v", bill["utm"])
	}

	// 13. Креатор видит свой заработок, и только свой.
	earn := w.step(t, "заработок креатора", http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID+"/earnings", w.crTk, nil, http.StatusOK)
	mineAcc, _ := earn["accruals"].([]any)
	if len(mineAcc) != 1 {
		t.Fatalf("креатор должен видеть своё начисление: %v", earn)
	}
	w.step(t, "чужой заработок", http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID+"/earnings", w.h.Token(t, other),
		nil, http.StatusNotFound)

	// 14. Границы доступа: клиент не ходит менеджерскими ручками, креатор
	// не читает клиентский счёт.
	w.step(t, "клиент в деньгах менеджера", http.MethodGet,
		"/api/v1/manager/projects/"+projectID+"/billing", w.clientTk, nil, http.StatusForbidden)
	w.step(t, "креатор в счёте клиента", http.MethodGet,
		"/api/v1/me/projects/"+projectID+"/billing", w.crTk, nil, http.StatusNotFound)
}
