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
	"marketpclce/tests/integration"
)

// Справочник порогов оценок: версионируется как прайс.
//
// Смысл версий в одном: поправили порог — прошлое не переписалось. Проект
// снимает копию действующей версии, подытоженный период помнит свою, и
// выпуск новой их не трогает.

// Значения по умолчанию — те, что пришли из аналитики. Мигрaция заводит
// первую версию сама: без действующей версии справочник пуст, и первый
// же вопрос упрётся в «нет данных» на ровном месте.
func TestRatingScaleDefaults(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)

	code, body := s.get(t, "/api/v1/admin/rating_scales/current")
	if code != http.StatusOK {
		t.Fatalf("действующая версия: код %d, тело %v", code, body)
	}
	levels := subMap(t, body, "levels")
	for _, c := range []struct {
		key  string
		want int
	}{
		{"medium_from", 2000},
		{"good_from", 6000},
		{"great_from", 15000},
		{"hit_from", 50000},
		{"viral_from", 300000},
	} {
		if got := num(t, levels, c.key); got != c.want {
			t.Errorf("порог %s = %d, ожидали %d", c.key, got, c.want)
		}
	}
	if got := num(t, body, "typical_video_views"); got != 3000 {
		t.Errorf("типичный ролик %d, ожидали 3000", got)
	}

	shares := subMap(t, body, "shares")
	if num(t, shares, "bad_pct") != 30 || num(t, shares, "medium_pct") != 45 ||
		num(t, shares, "good_pct") != 15 || num(t, shares, "great_pct") != 10 {
		t.Errorf("норма долей: %v", shares)
	}
	rel := subMap(t, body, "relative")
	if num(t, rel, "window_days") != 90 || num(t, rel, "min_mature_videos") != 100 ||
		num(t, rel, "mature_age_days") != 14 {
		t.Errorf("параметры относительной оценки: %v", rel)
	}

	// Площадки: все пять со своими числами.
	want := map[string][3]int{
		"youtube":   {550, 2800, 9000},
		"instagram": {160, 1700, 3000},
		"tiktok":    {630, 930, 1800},
		"likee":     {350, 880, 1000},
		"vk":        {2, 16, 140},
	}
	items := list(t, body, "platform_levels")
	if len(items) != 5 {
		t.Fatalf("площадок в справочнике %d, ожидали пять", len(items))
	}
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		platform, _ := m["platform"].(string)
		w, ok := want[platform]
		if !ok {
			t.Errorf("чужая площадка в справочнике: %q", platform)
			continue
		}
		if num(t, m, "medium_from") != w[0] || num(t, m, "good_from") != w[1] ||
			num(t, m, "great_from") != w[2] {
			t.Errorf("пороги %s: %v, ожидали %v", platform, m, w)
		}
	}

	// Ориентиры рынка: у каждого источник и дата — без них число через
	// год начнёт врать, и заметить это будет нечем.
	market := list(t, body, "market")
	if len(market) == 0 {
		t.Fatal("ориентиров рынка нет вовсе")
	}
	for _, raw := range market {
		m, _ := raw.(map[string]any)
		if src, _ := m["source"].(string); src == "" {
			t.Errorf("ориентир %v без источника", m["key"])
		}
		if day, _ := m["measured_on"].(string); day == "" {
			t.Errorf("ориентир %v без даты измерения", m["key"])
		}
	}
}

// Выпуск новой версии не трогает старые и попадает в журнал.
func TestRatingScalePublishKeepsPrevious(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	s := newAdminShell(t, pool)

	_, before := s.get(t, "/api/v1/admin/rating_scales/current")
	prevID, _ := before["id"].(string)
	prevVersion := num(t, before, "version")

	req := publishScaleReq()
	req["note"] = "подняли порог хита"
	code, body := s.post(t, "/api/v1/admin/rating_scales", req)
	if code != http.StatusCreated {
		t.Fatalf("выпуск версии: код %d, тело %v", code, body)
	}
	scale := subMap(t, body, "scale")
	newID, _ := scale["id"].(string)
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM admin_audit_log WHERE object_type = $1 AND object_id = $2`,
			audit.ObjectRatingScale, newID)
		_, _ = pool.Exec(ctx, `DELETE FROM rating_scales WHERE id = $1`, newID)
	})
	if got := num(t, scale, "version"); got != prevVersion+1 {
		t.Errorf("версия %d, ожидали %d", got, prevVersion+1)
	}
	if v, _ := scale["is_current"].(bool); !v {
		t.Error("свежая версия не помечена действующей")
	}

	// Разница с прежней — как у прайса.
	changes := list(t, body, "changes")
	if len(changes) == 0 {
		t.Error("выпуск не показал, что изменилось")
	}

	// Прежняя версия осталась как была.
	_, list0 := s.get(t, "/api/v1/admin/rating_scales")
	for _, raw := range list(t, list0, "items") {
		m, _ := raw.(map[string]any)
		if m["id"] != prevID {
			continue
		}
		if num(t, subMap(t, m, "levels"), "hit_from") != 50000 {
			t.Error("выпуск новой версии переписал прежнюю")
		}
		if v, _ := m["is_current"].(bool); v {
			t.Error("прежняя версия всё ещё помечена действующей")
		}
	}

	// Выпуск — админское действие, и след у него такой же обязательный,
	// как у выпуска прайса.
	if n := auditCountAction(t, pool, audit.ActionRatingScalePublish, newID); n != 1 {
		t.Errorf("записей в журнале %d, ожидали 1", n)
	}
}

// Проект снимает копию действующей версии и за новой не следует.
func TestProjectKeepsItsRatingScale(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	s := newAdminShell(t, pool)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	// Копия снимается при первом вопросе об оценке — например, когда
	// креатор открывает кабинет.
	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.CreatorEarnings(ctx, pid, creators[0], time.Now().UTC()); err != nil {
		t.Fatalf("кабинет креатора: %v", err)
	}
	attached := projectScaleVersion(t, pool, pid)
	if attached == 0 {
		t.Fatal("проект не снял копию справочника")
	}

	// Выпускаем новую версию с другим типичным роликом.
	req := publishScaleReq()
	req["typical_video_views"] = 7777
	code, body := s.post(t, "/api/v1/admin/rating_scales", req)
	if code != http.StatusCreated {
		t.Fatalf("выпуск версии: код %d, тело %v", code, body)
	}
	newID, _ := subMap(t, body, "scale")["id"].(string)
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM admin_audit_log WHERE object_type = $1 AND object_id = $2`,
			audit.ObjectRatingScale, newID)
		_, _ = pool.Exec(ctx, `DELETE FROM rating_scales WHERE id = $1`, newID)
	})

	if got := projectScaleVersion(t, pool, pid); got != attached {
		t.Errorf("проект переехал на версию %d — копия снимается один раз", got)
	}
	// И «типичный ролик» у него остался прежним, а не 7777.
	earn, err := svc.CreatorEarnings(ctx, pid, creators[0], time.Now().UTC())
	if err != nil {
		t.Fatalf("кабинет креатора: %v", err)
	}
	if earn.Benchmark == nil {
		t.Fatal("в кабинете нет ориентира")
	}
	if earn.Benchmark.TypicalVideoViews == 7777 {
		t.Error("проект подхватил пороги новой версии — прошлое переписалось")
	}
}

// Подытоженный период помнит версию, которой его оценивали.
func TestLockedPeriodRemembersRatingScale(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}
	publishOn(t, pool, pid, creators[0], time.Now().UTC().AddDate(0, -2, 0), "scl01", 100_000)
	if _, _, err := svc.LockDuePeriods(ctx, time.Now().UTC(), billing.DefaultPeriodLockDelay); err != nil {
		t.Fatalf("подытог: %v", err)
	}

	p1, err := svc.Period(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if !p1.IsLocked() {
		t.Fatal("период не подытожен")
	}
	if p1.RatingScaleID == nil || p1.RatingScaleVersion == nil {
		t.Fatalf("подытоженный период не запомнил версию порогов: %+v", p1)
	}
	stamped := *p1.RatingScaleVersion

	// Идущий период версии ещё не имеет: пороги замораживаются подытогом.
	if p2, err := svc.Period(ctx, pid, 2, time.Now().UTC()); err == nil && p2.RatingScaleID != nil {
		t.Error("у идущего периода уже проставлена версия порогов")
	}

	// Новая версия старый период не трогает.
	s := newAdminShell(t, pool)
	code, body := s.post(t, "/api/v1/admin/rating_scales", publishScaleReq())
	if code != http.StatusCreated {
		t.Fatalf("выпуск версии: код %d, тело %v", code, body)
	}
	newID, _ := subMap(t, body, "scale")["id"].(string)
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM admin_audit_log WHERE object_type = $1 AND object_id = $2`,
			audit.ObjectRatingScale, newID)
		_, _ = pool.Exec(ctx, `DELETE FROM rating_scales WHERE id = $1`, newID)
	})
	p1, err = svc.Period(ctx, pid, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("период: %v", err)
	}
	if p1.RatingScaleVersion == nil || *p1.RatingScaleVersion != stamped {
		t.Errorf("версия подытоженного периода поехала: было %d, стало %v", stamped, p1.RatingScaleVersion)
	}
}

// Менеджера к справочнику не пускают: пороги решают, каким ролик назовут.
func TestRatingScalesAreAdminOnly(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	manager, cleanup := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanup()

	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/admin/rating_scales"},
		{http.MethodGet, "/api/v1/admin/rating_scales/current"},
		{http.MethodPost, "/api/v1/admin/rating_scales"},
	} {
		code, _ := h.Do(t, c.method, c.path, h.Token(t, manager), publishScaleReq())
		if code != http.StatusForbidden {
			t.Errorf("%s %s под менеджером: код %d, ожидали 403", c.method, c.path, code)
		}
	}
}

// Кривая версия не выпускается: границы должны расти, доли складываться
// в сто, площадки быть все, у ориентира — источник и дата.
func TestRatingScalePublishValidates(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)

	cases := []struct {
		name  string
		patch func(map[string]any)
	}{
		{"границы не растут", func(m map[string]any) {
			m["levels"] = map[string]any{
				"medium_from": 6000, "good_from": 2000, "great_from": 15000,
				"hit_from": 50000, "viral_from": 300000,
			}
		}},
		{"доли не сто", func(m map[string]any) {
			m["shares"] = map[string]any{
				"bad_pct": 30, "medium_pct": 45, "good_pct": 15, "great_pct": 5,
			}
		}},
		{"нет площадки", func(m map[string]any) {
			platforms, _ := m["platform_levels"].([]map[string]any)
			m["platform_levels"] = platforms[:2]
		}},
		{"ориентир без источника", func(m map[string]any) {
			m["market"] = []map[string]any{{
				"key": "bloggers", "title": "Реклама у блогеров",
				"price_per_1000": 30000, "source": "", "measured_on": "2026-09-01",
			}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := publishScaleReq()
			c.patch(req)
			code, body := s.post(t, "/api/v1/admin/rating_scales", req)
			if code != http.StatusBadRequest {
				t.Fatalf("код %d, ожидали 400 (тело %v)", code, body)
			}
			if msg, _ := body["message"].(string); msg == "" {
				t.Error("отказ без объяснения — админ не поймёт, что поправить")
			}
		})
	}
}

// ---- помощники ----

// publishScaleReq — корректное тело выпуска: те же числа, что в первой
// версии, кроме порога хита. Тесты меняют в нём по одному полю.
func publishScaleReq() map[string]any {
	return map[string]any{
		"note": "тестовая версия",
		"levels": map[string]any{
			"medium_from": 2000, "good_from": 6000, "great_from": 15000,
			"hit_from": 60000, "viral_from": 300000,
		},
		"typical_video_views": 3000,
		"shares": map[string]any{
			"bad_pct": 30, "medium_pct": 45, "good_pct": 15, "great_pct": 10,
		},
		"relative": map[string]any{
			"window_days": 90, "min_mature_videos": 100, "mature_age_days": 14,
		},
		"platform_levels": []map[string]any{
			{"platform": "youtube", "medium_from": 550, "good_from": 2800, "great_from": 9000},
			{"platform": "instagram", "medium_from": 160, "good_from": 1700, "great_from": 3000},
			{"platform": "tiktok", "medium_from": 630, "good_from": 930, "great_from": 1800},
			{"platform": "likee", "medium_from": 350, "good_from": 880, "great_from": 1000},
			{"platform": "vk", "medium_from": 2, "good_from": 16, "great_from": 140},
		},
		"market": []map[string]any{
			{"key": "bloggers", "title": "Реклама у блогеров", "price_per_1000": 30000,
				"source": "аналитика владельца продукта", "measured_on": "2026-09-01"},
		},
	}
}

func projectScaleVersion(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID) int {
	t.Helper()
	var version *int
	if err := pool.QueryRow(context.Background(), `
SELECT rs.version FROM projects p
LEFT JOIN rating_scales rs ON rs.id = p.rating_scale_id
WHERE p.id = $1`, pid).Scan(&version); err != nil {
		t.Fatalf("версия справочника проекта: %v", err)
	}
	if version == nil {
		return 0
	}
	return *version
}
