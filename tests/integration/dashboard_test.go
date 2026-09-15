package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/tests/integration"
)

// Дашборд заказчика: /me/overview с окном.
//
// Окно скользящее от сегодня: week — 7 дней, month — 30, quarter — 90.
// Итоги в ответе за всё время, приросты — окно против предыдущего окна
// такой же длины. Ноль и отсутствие прироста означают разное, и это
// главное, что здесь проверяется.

// dashDay — снимок статистики конкретной площадки конкретного ролика за
// день. Ряд накопительный: прирост считается как разница со вчерашним
// снимком той же ссылки.
func dashDay(t *testing.T, pool *pgxpool.Pool, pubID uuid.UUID, platform string,
	day time.Time, views, likes, comments int64, shares *int64) {

	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, shares, collected_at)
SELECT l.id, $3::date, $4, $5, $6, $7, $3::timestamptz
FROM publication_links l
WHERE l.publication_id = $1 AND l.platform = $2
ON CONFLICT (link_id, stat_date) DO UPDATE
SET views = EXCLUDED.views, likes = EXCLUDED.likes,
    comments = EXCLUDED.comments, shares = EXCLUDED.shares`,
		pubID, platform, day, views, likes, comments, shares); err != nil {
		t.Fatalf("снимок %s за %s: %v", platform, day.Format("2006-01-02"), err)
	}
}

func daysAgo(n int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n)
}

// dashboardBody — ответ дашборда за окно.
func dashboardBody(t *testing.T, h *integration.APIHarness, client uuid.UUID, rng string) map[string]any {
	t.Helper()
	path := "/api/v1/me/overview"
	if rng != "" {
		path += "?range=" + rng
	}
	raw := rawBody(t, h, path, h.Token(t, client))
	// Наших денег на дашборде быть не должно ни на какой глубине: блок
	// новый, а список запретных ключей обязан его накрывать.
	assertNoForbiddenKeys(t, raw, forbiddenForClient, allowedCreatorKeys)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("разобрать ответ: %v", err)
	}
	return body
}

func optNum(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func platformRow(t *testing.T, body map[string]any, platform string) map[string]any {
	t.Helper()
	for _, raw := range list(t, body, "platforms") {
		m, _ := raw.(map[string]any)
		if m["platform"] == platform {
			return m
		}
	}
	t.Fatalf("площадки %q нет в ответе", platform)
	return nil
}

// Прирост считается против предыдущего окна той же длины.
func TestDashboardDeltaAgainstPreviousWindow(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "dsh1")
	defer cleanup()

	var pubID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM project_publications WHERE project_id = $1 LIMIT 1`, pid).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	resetDailyViews(t, pool, pid)

	sh := func(v int64) *int64 { return &v }
	// Опорный снимок до обоих окон: без него весь накопленный объём
	// засчитался бы приростом первого дня прошлого окна.
	dashDay(t, pool, pubID, "tiktok", daysAgo(70), 0, 0, 0, sh(0))
	// Прошлое окно месяца (с 59-го по 30-й день назад): +100 000.
	dashDay(t, pool, pubID, "tiktok", daysAgo(45), 100_000, 1_000, 100, sh(50))
	// Текущее окно: +200 000, то есть вдвое больше.
	dashDay(t, pool, pubID, "tiktok", daysAgo(15), 300_000, 4_000, 400, sh(150))

	body := dashboardBody(t, h, client, "month")

	if body["range"] != "month" {
		t.Errorf("окно %v, ожидалось month", body["range"])
	}
	if body["range_label"] == "" {
		t.Error("подписи окна нет")
	}
	// Итог — за всё время: по нему считается стоимость тысячи, и
	// подменять его оконным нельзя.
	if got := num(t, subMap(t, body, "views"), "total"); got != 300_000 {
		t.Errorf("просмотров всего %d, ожидалось 300 000", got)
	}
	if got := num(t, subMap(t, body, "engagement"), "total"); got != 4_000+400+150 {
		t.Errorf("взаимодействий всего %d, ожидалось %d", got, 4_000+400+150)
	}

	// Заголовок дашборда и его прирост описывают ОДНУ величину — окно.
	win := subMap(t, body, "window")
	if got := num(t, win, "views"); got != 200_000 {
		t.Errorf("просмотров за окно %d, ожидалось 200 000", got)
	}
	delta, ok := optNum(win, "views_delta_pct")
	if !ok {
		t.Fatal("прироста просмотров нет, а прошлое окно есть")
	}
	if delta != 100 {
		t.Errorf("прирост %v%%, ожидалось 100 (200 000 против 100 000)", delta)
	}
	// Взаимодействия окна: прирост лайков, комментариев и репостов.
	if got := num(t, win, "engagement"); got != (4_000-1_000)+(400-100)+(150-50) {
		t.Errorf("взаимодействий за окно %d, ожидалось %d", got, 3_000+300+100)
	}
	if got := num(t, win, "comments"); got != 300 {
		t.Errorf("комментариев за окно %d, ожидалось 300", got)
	}
	if _, ok := optNum(win, "engagement_delta_pct"); !ok {
		t.Error("прироста взаимодействий нет, а прошлое окно есть")
	}
	if _, ok := optNum(win, "comments_delta_pct"); !ok {
		t.Error("прироста комментариев нет, а прошлое окно есть")
	}

	// Площадка: прирост тот же, ряд — только дни со сбором.
	tt := platformRow(t, body, "tiktok")
	if got, _ := optNum(tt, "delta_pct"); got != 100 {
		t.Errorf("прирост tiktok %v%%, ожидалось 100", got)
	}
	series, _ := tt["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("в ряду %d точек, ожидалась одна: дни без сбора пропускаются, а не отдаются нулём", len(series))
	}
	point, _ := series[0].(map[string]any)
	if got := num(t, point, "views_gained"); got != 200_000 {
		t.Errorf("прирост в точке ряда %d, ожидалось 200 000", got)
	}
	if point["date"] != daysAgo(15).Format("2006-01-02") {
		t.Errorf("дата точки %v, ожидалась %s", point["date"], daysAgo(15).Format("2006-01-02"))
	}
}

// Когда сравнивать не с чем, поля прироста нет ВОВСЕ. Ноль означал бы
// «не выросло» — на продающем экране это другое утверждение.
func TestDashboardWithoutPreviousWindowHasNoDelta(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "dsh2")
	defer cleanup()

	var pubID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM project_publications WHERE project_id = $1 LIMIT 1`, pid).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	resetDailyViews(t, pool, pid)
	// Всё случилось внутри текущего окна: прошлого нет.
	dashDay(t, pool, pubID, "tiktok", daysAgo(3), 50_000, 500, 50, nil)

	body := dashboardBody(t, h, client, "month")

	win := subMap(t, body, "window")
	if _, ok := optNum(win, "views_delta_pct"); ok {
		t.Error("прирост просмотров есть, хотя сравнивать не с чем")
	}
	if _, ok := optNum(win, "engagement_delta_pct"); ok {
		t.Error("прирост взаимодействий есть, хотя сравнивать не с чем")
	}
	if _, ok := optNum(win, "comments_delta_pct"); ok {
		t.Error("прирост комментариев есть, хотя сравнивать не с чем")
	}
	if _, ok := optNum(win, "er_delta_pp"); ok {
		t.Error("изменение вовлечённости есть, хотя сравнивать не с чем")
	}
	if _, ok := optNum(platformRow(t, body, "tiktok"), "delta_pct"); ok {
		t.Error("прирост площадки есть, хотя сравнивать не с чем")
	}

	// Зато звёздочка про репосты на месте — и у окна, и за всё время:
	// площадка их не отдала.
	if er := subMap(t, body, "er"); er["without_shares"] != true {
		t.Error("репостов нет, а признака without_shares тоже нет")
	}
	if win["er_without_shares"] != true {
		t.Error("в окне репостов нет, а признака er_without_shares тоже нет")
	}
}

// Пять площадок всегда, по убыванию просмотров, доли дают сто процентов.
func TestDashboardPlatformsAlwaysFiveAndSorted(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "dsh3")
	defer cleanup()

	var pubID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM project_publications WHERE project_id = $1 LIMIT 1`, pid).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	resetDailyViews(t, pool, pid)
	sh := func(v int64) *int64 { return &v }
	dashDay(t, pool, pubID, "tiktok", daysAgo(5), 400_000, 10_000, 1_000, sh(500))
	dashDay(t, pool, pubID, "youtube", daysAgo(5), 300_000, 5_000, 500, sh(250))
	dashDay(t, pool, pubID, "vk", daysAgo(5), 200_000, 2_000, 200, sh(100))
	dashDay(t, pool, pubID, "instagram", daysAgo(5), 100_000, 1_000, 100, sh(50))
	// likee не собирали вовсе — площадка обязана остаться в ответе с нулём.

	body := dashboardBody(t, h, client, "month")

	rows := list(t, body, "platforms")
	if len(rows) != 5 {
		t.Fatalf("площадок %d, ожидалось пять всегда", len(rows))
	}
	var (
		prev     = int64(1 << 62)
		sumShare int
		seen     = map[string]bool{}
	)
	for _, raw := range rows {
		m, _ := raw.(map[string]any)
		name, _ := m["platform"].(string)
		seen[name] = true
		v := int64(num(t, m, "views"))
		if v > prev {
			t.Errorf("площадки идут не по убыванию просмотров: %s = %d после %d", name, v, prev)
		}
		prev = v
		sumShare += num(t, m, "share_pct")
	}
	for _, p := range []string{"tiktok", "instagram", "youtube", "vk", "likee"} {
		if !seen[p] {
			t.Errorf("в ответе нет площадки %q", p)
		}
	}
	// Округление даёт погрешность в пару процентов — больше быть не должно.
	if sumShare < 98 || sumShare > 102 {
		t.Errorf("сумма долей площадок %d%%, ожидалось около ста", sumShare)
	}

	// Вовлечённость считается с репостами, где они есть.
	tt := platformRow(t, body, "tiktok")
	er, ok := optNum(tt, "er_percent")
	if !ok {
		t.Fatal("у tiktok нет вовлечённости, хотя просмотры есть")
	}
	want := float64(10_000+1_000+500) / float64(400_000) * 100
	if er < want-0.1 || er > want+0.1 {
		t.Errorf("вовлечённость tiktok %v%%, ожидалось около %.1f", er, want)
	}
	if tt["er_without_shares"] == true {
		t.Error("у tiktok репосты есть, а стоит признак «без репостов»")
	}
}

// «Лучшее за окно» — только свои ролики, по убыванию прироста.
func TestDashboardTopVideosOwnProjectsOnly(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	mine, cleanupMine := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "dsh4")
	defer cleanupMine()

	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupStranger()
	foreign, cleanupForeign := overviewProject(t, pool, stranger, 6_000_000, 9_000, 0, "dsh5")
	defer cleanupForeign()

	pubOf := func(pid uuid.UUID) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT id FROM project_publications WHERE project_id = $1 LIMIT 1`, pid).Scan(&id); err != nil {
			t.Fatalf("выкладка: %v", err)
		}
		return id
	}
	myPub, foreignPub := pubOf(mine), pubOf(foreign)
	resetDailyViews(t, pool, mine)
	resetDailyViews(t, pool, foreign)

	dashDay(t, pool, myPub, "tiktok", daysAgo(4), 120_000, 0, 0, nil)
	// У чужого ролика прирост больше — в наш дашборд он попасть не должен.
	dashDay(t, pool, foreignPub, "tiktok", daysAgo(4), 900_000, 0, 0, nil)

	if _, err := pool.Exec(ctx,
		`UPDATE project_publications SET title = 'Мой ролик' WHERE id = $1`, myPub); err != nil {
		t.Fatalf("название: %v", err)
	}

	body := dashboardBody(t, h, client, "month")
	top := list(t, body, "top_videos")
	if len(top) != 1 {
		t.Fatalf("роликов в top_videos %d, ожидался один свой", len(top))
	}
	v, _ := top[0].(map[string]any)
	if v["publication_id"] != myPub.String() {
		t.Errorf("в top_videos чужой ролик: %v", v["publication_id"])
	}
	if v["title"] != "Мой ролик" {
		t.Errorf("название %v, ожидалось «Мой ролик»", v["title"])
	}
	if got := num(t, v, "views"); got != 120_000 {
		t.Errorf("просмотров за окно %d, ожидалось 120 000", got)
	}
	if v["platform"] != "tiktok" {
		t.Errorf("площадка-лидер %v, ожидался tiktok", v["platform"])
	}
	if v["url"] == "" {
		t.Error("ссылки на ролик нет")
	}
	if v["published_at"] == "" {
		t.Error("даты выхода нет")
	}
}

// Смена окна меняет и числа, и подпись.
func TestDashboardRangeChangesNumbersAndLabel(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "dsh6")
	defer cleanup()

	var pubID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM project_publications WHERE project_id = $1 LIMIT 1`, pid).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	resetDailyViews(t, pool, pid)
	dashDay(t, pool, pubID, "tiktok", daysAgo(60), 0, 0, 0, nil)
	// Прирост случился 20 дней назад: он внутри месяца и квартала, но
	// вне недели.
	dashDay(t, pool, pubID, "tiktok", daysAgo(20), 500_000, 0, 0, nil)

	week := dashboardBody(t, h, client, "week")
	month := dashboardBody(t, h, client, "month")

	weekSeries, _ := platformRow(t, week, "tiktok")["series"].([]any)
	monthSeries, _ := platformRow(t, month, "tiktok")["series"].([]any)
	if len(weekSeries) != 0 {
		t.Errorf("в неделе %d точек, ожидалось ноль: прирост был раньше", len(weekSeries))
	}
	if len(monthSeries) != 1 {
		t.Errorf("в месяце %d точек, ожидалась одна", len(monthSeries))
	}
	if week["range_label"] == month["range_label"] {
		t.Errorf("подпись окна не поменялась: %v", week["range_label"])
	}
	if week["range"] != "week" || month["range"] != "month" {
		t.Errorf("окна в ответе: %v и %v", week["range"], month["range"])
	}

	// Незнакомое окно — отказ с перечислением допустимых, а не тихая
	// подмена на месяц.
	code, body := h.Do(t, http.MethodGet, "/api/v1/me/overview?range=year", h.Token(t, client), nil)
	if code != http.StatusBadRequest {
		t.Errorf("код %d, ожидался 400 (%v)", code, body)
	}
}

// Сравнение с рынком: только когда есть своя цена тысячи, с источником и
// датой у каждой цифры.
func TestDashboardMarketComparison(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	_, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 1_000_000, "dsh7")
	defer cleanup()

	body := dashboardBody(t, h, client, "month")
	cost, ok := optNum(body, "cost_per_1000")
	if !ok || cost <= 0 {
		t.Fatalf("своей цены тысячи нет: %v", body["cost_per_1000"])
	}
	items := list(t, body, "market")
	if len(items) == 0 {
		t.Fatal("сравнения с рынком нет, хотя цена тысячи есть")
	}
	if num(t, body, "market_scale_version") <= 0 {
		t.Error("версии справочника, из которой взяты ориентиры, в ответе нет")
	}
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		if m["source"] == "" || m["source"] == nil {
			t.Errorf("у ориентира %v нет источника", m["key"])
		}
		if m["measured_on"] == "" || m["measured_on"] == nil {
			t.Errorf("у ориентира %v нет даты замера", m["key"])
		}
		price, _ := optNum(m, "price_per_1000")
		times, _ := optNum(m, "times_cheaper")
		want := price / cost
		if times < want-0.15 || times > want+0.15 {
			t.Errorf("%v: во сколько раз дороже — %v, ожидалось около %.1f (считается от фактической цены)",
				m["key"], times, want)
		}
	}

	// Заказчик без просмотров: делить не на что, и блока быть не должно.
	empty, cleanupEmpty := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupEmpty()
	_, cleanupProject := emptyClientProject(t, pool, empty)
	defer cleanupProject()

	none := dashboardBody(t, h, empty, "month")
	if _, ok := none["market"]; ok {
		t.Error("сравнение с рынком отдано без своей цены тысячи — делить не на что")
	}
	if _, ok := none["market_scale_version"]; ok {
		t.Error("версия справочника отдана без самого сравнения")
	}
}

// Оконная сумма НЕ РАВНА общей, когда есть история старше окна.
//
// Тест стоит отдельно намеренно: заголовок дашборда берёт window.views, а
// стоимость работы и цена тысячи — views.total за всё время. «Починить»
// одно в другое очень легко, и на данных без истории подмена не видна.
func TestDashboardWindowViewsDifferFromAllTime(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	pid, cleanup := overviewProject(t, pool, client, 6_000_000, 9_000, 0, "dsh8")
	defer cleanup()

	var pubID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM project_publications WHERE project_id = $1 LIMIT 1`, pid).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	resetDailyViews(t, pool, pid)
	// Миллион набран задолго до окна, и ещё двести тысяч — внутри.
	dashDay(t, pool, pubID, "tiktok", daysAgo(120), 1_000_000, 0, 0, nil)
	dashDay(t, pool, pubID, "tiktok", daysAgo(10), 1_200_000, 0, 0, nil)

	body := dashboardBody(t, h, client, "month")

	total := num(t, subMap(t, body, "views"), "total")
	window := num(t, subMap(t, body, "window"), "views")
	if total != 1_200_000 {
		t.Errorf("просмотров всего %d, ожидалось 1 200 000", total)
	}
	if window != 200_000 {
		t.Errorf("просмотров за окно %d, ожидалось 200 000", window)
	}
	if total == window {
		t.Fatal("оконная сумма совпала с общей — одно «починили» в другое")
	}

	// У площадки — обе величины рядом, и оконные доли считаются от
	// оконной суммы.
	tt := platformRow(t, body, "tiktok")
	if got := num(t, tt, "views"); int64(got) != int64(total) {
		t.Errorf("просмотры площадки за всё время %d, ожидалось %d", got, total)
	}
	if got := num(t, tt, "window_views"); int64(got) != int64(window) {
		t.Errorf("просмотры площадки за окно %d, ожидалось %d", got, window)
	}
	if got := num(t, tt, "window_share_pct"); got != 100 {
		t.Errorf("оконная доля единственной площадки %d%%, ожидалось 100", got)
	}
	var sumWindowShare int
	for _, raw := range list(t, body, "platforms") {
		m, _ := raw.(map[string]any)
		sumWindowShare += num(t, m, "window_share_pct")
	}
	if sumWindowShare < 98 || sumWindowShare > 102 {
		t.Errorf("сумма оконных долей %d%%, ожидалось около ста", sumWindowShare)
	}
}
