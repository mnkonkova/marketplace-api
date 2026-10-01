package integration_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Справочники должны показывать, что стоит за строкой.
//
// Прайс, чеклисты, воронки и продакшены правят и выключают, а последствия
// правки до сих пор были не видны: сколько проектов идёт по этой воронке,
// скольким клиентам придётся заново согласовывать цену. Решение принимали
// на ощупь, а узнавали о последствиях после.

// Выпуск версии прайса показывает разницу с действующей и сколько
// клиентов должны согласиться заново.
func TestPublishTermsShowsChangesAndConsents(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	base := map[string]any{
		"salary_per_month":         6_000_000,
		"videos_first_month":       30,
		"videos_next_months":       60,
		"rate_per_1000_views":      9_000,
		"bonus_views_threshold":    1_000_000,
		"rate_per_1000_views_over": 900,
		"creator_salary_per_month": 4_500_000,
		"body":                     "Условия, версия для теста справочников.",
	}
	code, first := s.post(t, "/api/v1/admin/terms", base)
	if code != http.StatusCreated {
		t.Fatalf("выпуск первой версии: код %d, тело %v", code, first)
	}
	firstID, _ := first["terms_version_id"].(string)
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM client_terms_consents WHERE terms_version_id = $1`, firstID)
		_, _ = pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, firstID)
	})

	// Маржа площадки считается, а не хранится: две колонки ставок рядом
	// админ иначе вычитает в уме на каждой строке.
	margin := subMap(t, first, "margin")
	if got := num(t, margin, "salary_per_month"); got != 1_500_000 {
		t.Errorf("маржа по окладу %d, ожидали 1500000", got)
	}
	if v, _ := margin["has_margin"].(bool); !v {
		t.Error("креаторская ставка заполнена, а has_margin=false")
	}

	// Клиент согласился именно с этой версией — значит после выпуска
	// следующей его нужно спросить заново.
	client := s.user(t, userOpts{Kind: "client"})
	if _, err := pool.Exec(ctx, `
INSERT INTO client_terms_consents (user_id, terms_version_id) VALUES ($1, $2)`,
		client, firstID); err != nil {
		t.Fatalf("согласие клиента: %v", err)
	}

	next := map[string]any{}
	for k, v := range base {
		next[k] = v
	}
	next["rate_per_1000_views"] = 10_000
	next["body"] = "Условия, вторая версия для теста."
	code, second := s.post(t, "/api/v1/admin/terms", next)
	if code != http.StatusCreated {
		t.Fatalf("выпуск второй версии: код %d, тело %v", code, second)
	}
	secondID, _ := second["terms_version_id"].(string)
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, secondID)
	})

	if got := num(t, second, "consents_required"); got != 1 {
		t.Errorf("согласий заново %d, ожидали 1: %v", got, second)
	}
	changes := list(t, second, "changes")
	if len(changes) != 1 {
		t.Fatalf("изменений %d, ожидали одно (ставка): %v", len(changes), changes)
	}
	ch, _ := changes[0].(map[string]any)
	if ch["field"] != "rate_per_1000_views" {
		t.Errorf("изменилось поле %v, ожидали rate_per_1000_views", ch["field"])
	}
	if num(t, ch, "from") != 9_000 || num(t, ch, "to") != 10_000 {
		t.Errorf("разница %v → %v, ожидали 9000 → 10000", ch["from"], ch["to"])
	}
}

// Шаблон чеклиста знает, на скольких проектах он подключён: новая версия
// не тронет уже снятые снимки, и это надо видеть ДО выпуска.
func TestChecklistTemplateKnowsItsProjects(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	name := "R2 чеклист " + uuid.NewString()[:8]
	code, tpl := s.post(t, "/api/v1/admin/checklist_templates", map[string]any{
		"name":  name,
		"items": []map[string]any{{"text": "Обложка", "is_required": true}},
	})
	if code != http.StatusCreated {
		t.Fatalf("выпуск шаблона: код %d, тело %v", code, tpl)
	}
	tplID, _ := tpl["id"].(string)
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tplID)
	})
	if got := num(t, tpl, "projects_count"); got != 0 {
		t.Errorf("свежий шаблон подключён к %d проектам", got)
	}

	client := s.user(t, userOpts{Kind: "client"})
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 чеклист на проекте",
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO project_checklist_snapshot (project_id, template_id, template_name, template_version)
VALUES ($1, $2, $3, 1)`, pid, tplID, name); err != nil {
		t.Fatalf("подключить шаблон: %v", err)
	}
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM project_checklist_snapshot WHERE project_id = $1`, pid)
	})

	_, body := s.get(t, "/api/v1/admin/checklist_templates/"+tplID)
	if got := num(t, body, "projects_count"); got != 1 {
		t.Errorf("подключений у шаблона %d, ожидали 1", got)
	}
	_, body = s.get(t, "/api/v1/admin/checklist_templates")
	for _, raw := range list(t, body, "items") {
		if m, _ := raw.(map[string]any); m["id"] == tplID {
			if got := num(t, m, "projects_count"); got != 1 {
				t.Errorf("в списке подключений %d, ожидали 1", got)
			}
		}
	}
}

// Воронка показывает, сколько проектов по ней сейчас идёт: выключать её
// вслепую — значит узнать о двадцати проектах после клика.
func TestPipelineListShowsActiveProjects(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	client, pipelineID, projectID, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	_ = client
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET status = 'active' WHERE id = $1`, projectID); err != nil {
		t.Fatalf("перевести проект в работу: %v", err)
	}

	_, body := s.get(t, "/api/v1/admin/pipelines")
	found := false
	for _, raw := range list(t, body, "items") {
		m, _ := raw.(map[string]any)
		if m["id"] == pipelineID.String() {
			found = true
			if got := num(t, m, "active_projects"); got != 1 {
				t.Errorf("активных проектов у воронки %d, ожидали 1", got)
			}
		}
	}
	if !found {
		t.Errorf("воронка не нашлась в списке: %v", body)
	}
}

// Продакшен показывает участников и их активные проекты — тем же
// вопросом «можно ли выключить».
func TestProductionListShowsMembersAndProjects(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	name := "R2 продакшен " + uuid.NewString()[:8]
	var prodID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO productions (name) VALUES ($1) RETURNING id`, name).Scan(&prodID); err != nil {
		t.Fatalf("создать продакшен: %v", err)
	}
	s.defer_(func() { _, _ = pool.Exec(ctx, `DELETE FROM productions WHERE id = $1`, prodID) })

	specialist := s.user(t, userOpts{Kind: "specialist"})
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, production_id, is_published, moderation_status)
VALUES ($1, $2, $3, TRUE, 'approved')
ON CONFLICT (user_id) DO UPDATE SET production_id = EXCLUDED.production_id`,
		specialist, name+" участник", prodID); err != nil {
		t.Fatalf("профиль участника: %v", err)
	}
	client := s.user(t, userOpts{Kind: "client"})
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, SpecialistUserID: &specialist, Title: "R2 проект продакшена",
	})
	if _, err := pool.Exec(ctx, `UPDATE projects SET status = 'active' WHERE id = $1`, pid); err != nil {
		t.Fatalf("перевести проект в работу: %v", err)
	}

	_, body := s.get(t, "/api/v1/admin/productions")
	for _, raw := range list(t, body, "items") {
		m, _ := raw.(map[string]any)
		if m["id"] != prodID.String() {
			continue
		}
		if got := num(t, m, "members"); got != 1 {
			t.Errorf("участников %d, ожидали 1", got)
		}
		if got := num(t, m, "active_projects"); got != 1 {
			t.Errorf("активных проектов %d, ожидали 1", got)
		}
		return
	}
	t.Errorf("продакшен не нашёлся в справочнике: %v", body)
}

// Поиск людей для состава проекта: по идентификатору и с лицом.
//
// Менеджер набирает состав, узнавая людей в лицо, а список из одних
// почт заставляет читать каждую строку. И приходит он сюда часто со
// ссылкой или строкой из лога, где кроме uuid ничего нет — по такой
// строке раньше не находилось вообще ничего.
func TestSearchUsersByIDAndAvatar(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := integration.NewAPIHarness(t, pool)

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	token := h.Token(t, manager)

	creator, cleanupCreator := h.NewUser(t, userOpts{Kind: "specialist"})
	defer cleanupCreator()
	const avatar = "https://cdn.example.com/ava/searched.jpg"
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, avatar_url)
VALUES ($1, 'Аня Поисковая', $2)
ON CONFLICT (user_id) DO UPDATE SET display_name = EXCLUDED.display_name,
                                    avatar_url = EXCLUDED.avatar_url`,
		creator, avatar); err != nil {
		t.Fatalf("профиль: %v", err)
	}

	find := func(q string) []map[string]any {
		t.Helper()
		code, body := h.Do(t, http.MethodGet,
			"/api/v1/manager/users/search?kind=specialist&q="+url.QueryEscape(q), token, nil)
		if code != http.StatusOK {
			t.Fatalf("поиск %q: код %d", q, code)
		}
		raw, _ := body["items"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}

	mine := func(items []map[string]any) map[string]any {
		for _, it := range items {
			if it["user_id"] == creator.String() {
				return it
			}
		}
		return nil
	}

	// По имени — как было, но теперь с лицом.
	byName := mine(find("Аня Поисковая"))
	if byName == nil {
		t.Fatal("по имени человек не нашёлся")
	}
	if byName["avatar_url"] != avatar {
		t.Errorf("аватарка не отдана: %v", byName["avatar_url"])
	}

	// По НАЧАЛУ идентификатора: полный uuid руками никто не набирает.
	head := creator.String()[:8]
	if mine(find(head)) == nil {
		t.Errorf("по началу идентификатора %q человек не нашёлся", head)
	}

	// И по идентификатору целиком — его копируют из ссылки.
	if mine(find(creator.String())) == nil {
		t.Errorf("по полному идентификатору человек не нашёлся")
	}

	// Чужой идентификатор ничего не находит: поиск по префиксу не
	// должен превращаться в «покажи всех».
	if items := find("00000000-0000-0000-0000-0000000000ff"); len(items) != 0 {
		t.Errorf("несуществующий идентификатор нашёл %d человек", len(items))
	}
}
