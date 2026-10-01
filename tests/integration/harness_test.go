package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/tests/integration"
)

// Харнесс общий (tests/integration/harness.go): им пользуются и эти
// тесты, и сквозные сценарии из tests/e2e.
type apiHarness = integration.APIHarness

type userOpts = integration.UserOpts

func newAPIHarness(t *testing.T, pool *pgxpool.Pool) *apiHarness {
	return integration.NewAPIHarness(t, pool)
}

// Граница доступа к /manager/*. Это то, что не проверял ни один тест:
// сервис не отличает заказчика от менеджера, за это отвечает middleware.
func TestHarnessManagerRoutesRoleGate(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupClient()
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	pending, cleanupPending := h.NewUser(t, userOpts{Kind: "client", IsManager: true, NotApprove: true})
	defer cleanupPending()
	blocked, cleanupBlocked := h.NewUser(t, userOpts{Kind: "client", IsManager: true, NotActive: true})
	defer cleanupBlocked()

	const path = "/api/v1/manager/projects/inbox"

	cases := []struct {
		name     string
		token    string
		wantCode int
		wantErr  string
	}{
		{"без токена", "", http.StatusUnauthorized, "missing_bearer"},
		{"мусор вместо токена", "not-a-jwt", http.StatusUnauthorized, "invalid_token"},
		{"заказчик", h.Token(t, client), http.StatusForbidden, "forbidden_role"},
		{"неодобренный менеджер", h.Token(t, pending), http.StatusForbidden, "forbidden_unapproved"},
		{"деактивированный менеджер", h.Token(t, blocked), http.StatusForbidden, "inactive"},
		{"менеджер", h.Token(t, manager), http.StatusOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.Do(t, http.MethodGet, path, c.token, nil)
			if code != c.wantCode {
				t.Fatalf("код %d, ожидался %d. тело: %v", code, c.wantCode, body)
			}
			if c.wantErr != "" {
				if got, _ := body["error"].(string); got != c.wantErr {
					t.Errorf("код ошибки %q, ожидался %q", got, c.wantErr)
				}
			}
			// Любой отказ обязан объяснять человеку, что произошло.
			// Раньше "inactive" и "forbidden_role" отдавались без message,
			// и заказчик, ткнувшийся в менеджерский URL, получал голое
			// {"error":"forbidden_role"} — теперь выровнено.
			if code != http.StatusOK {
				if msg, _ := body["message"].(string); msg == "" {
					t.Error("отказ без объяснения — человек не поймёт, что делать")
				}
			}
		})
	}
}

// Токен, выписанный до смены пароля, больше не работает. Проверка отзыва
// живёт в middleware и раньше не была покрыта.
func TestHarnessTokenRevokedAfterPasswordChange(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	manager, cleanup := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanup()

	// password_changed_at по умолчанию now(), поэтому «старый» токен
	// отозвался бы сразу. Отодвигаем отметку в прошлое: нам нужен токен,
	// выписанный ПОСЛЕ последней смены пароля.
	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE users SET password_changed_at = now() - interval '2 hours' WHERE id = $1`,
		manager); err != nil {
		t.Fatalf("backdate password_changed_at: %v", err)
	}
	// Минуту назад, а не час: TTL access-токена 15 минут, часовой был бы
	// просто протухшим, и тест проверял бы не отзыв, а срок жизни.
	// Минуты хватает: отзыв срабатывает при разнице больше 1.5 секунды.
	issued := h.TokenAt(t, manager, time.Now().Add(-time.Minute))

	code, _ := h.Do(t, http.MethodGet, "/api/v1/manager/projects/inbox", issued, nil)
	if code != http.StatusOK {
		t.Fatalf("до смены пароля токен должен работать, а код %d", code)
	}

	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE users SET password_changed_at = now() WHERE id = $1`, manager); err != nil {
		t.Fatalf("change password: %v", err)
	}

	code, body := h.Do(t, http.MethodGet, "/api/v1/manager/projects/inbox", issued, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("после смены пароля код %d, ожидался 401. тело: %v", code, body)
	}
}

// Клиентские ручки не требуют роли, но обязаны отдавать только своё.
// Заказчик, запросивший чужой проект, получает 404, а не чужие данные.
func TestHarnessClientCannotReadForeignProject(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	_, _, projectID, cleanupProject := setupPipelineAndProject(t, pool)
	defer cleanupProject()

	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupStranger()

	code, body := h.Do(t, http.MethodGet,
		"/api/v1/me/projects/"+projectID.String()+"/funnel", h.Token(t, stranger), nil)
	if code != http.StatusNotFound {
		t.Fatalf("чужой проект: код %d, ожидался 404. тело: %v", code, body)
	}
}

// Выкладки через настоящий роутер: менеджер ставит даты, креатор сдаёт
// ссылки — с реальными токенами и проверкой роли на каждом шаге.
func TestHarnessPublicationsEndToEnd(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	managerToken := h.Token(t, manager)
	creatorToken := h.Token(t, creators[0])

	// Менеджер ставит даты пачкой.
	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+projectID.String()+"/publications/batch", managerToken,
		map[string]any{
			"creator_user_ids": []string{creators[0].String()},
			"scheme":           "tue_thu",
			"from":             "2026-09-01",
			"to":               "2026-09-14",
		})
	if code != http.StatusCreated {
		t.Fatalf("batch: код %d, тело %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("создано %d выкладок, ожидалось 4", len(items))
	}
	first, _ := items[0].(map[string]any)
	pubID, _ := first["id"].(string)

	// Креатор той же ручкой пользоваться не может — она менеджерская.
	code, _ = h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+projectID.String()+"/publications/batch", creatorToken,
		map[string]any{"creator_user_ids": []string{creators[0].String()},
			"scheme": "daily", "from": "2026-09-01", "to": "2026-09-02"})
	if code != http.StatusForbidden {
		t.Errorf("креатор в менеджерской ручке: код %d, ожидался 403", code)
	}

	// Креатор сдаёт ссылки своей ручкой.
	code, body = h.Do(t, http.MethodPost,
		"/api/v1/me/creator/publications/"+pubID+"/links", creatorToken,
		map[string]any{"urls": []string{
			"https://www.tiktok.com/@u/video/1?is_from_webapp=1",
			"https://youtu.be/dQw4w9WgXcQ",
		}})
	if code != http.StatusOK {
		t.Fatalf("submit links: код %d, тело %v", code, body)
	}
	if body["status"] != "partial" {
		t.Errorf("статус %v, ожидался partial", body["status"])
	}

	// Второй креатор проекта чужую выкладку не видит и сдать не может.
	code, _ = h.Do(t, http.MethodPost,
		"/api/v1/me/creator/publications/"+pubID+"/links", h.Token(t, creators[1]),
		map[string]any{"urls": []string{"https://vk.com/clip-1_2"}})
	if code != http.StatusNotFound {
		t.Errorf("чужая выкладка: код %d, ожидался 404", code)
	}

	// Менеджер видит все выкладки проекта.
	code, body = h.Do(t, http.MethodGet,
		"/api/v1/manager/projects/"+projectID.String()+"/publications", managerToken, nil)
	if code != http.StatusOK {
		t.Fatalf("manager list: код %d", code)
	}
	if all, _ := body["items"].([]any); len(all) != 4 {
		t.Errorf("менеджер видит %d выкладок, ожидалось 4", len(all))
	}
}

// Внутренний комментарий менеджера не должен доезжать до клиента.
// Это граница на выдаче: клиентская ручка обязана отфильтровать его в SQL,
// а не полагаться на то, что фронт его не покажет.
func TestHarnessInternalCommentHiddenFromClient(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	clientID, _, projectID, cleanupProject := setupPipelineAndProject(t, pool)
	defer cleanupProject()

	// Админ ходит менеджерскими URL: effectiveOwnerID пропускает его мимо
	// фильтра «назначен на проект». Эта ветка тоже не была покрыта.
	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	defer cleanupAdmin()
	adminToken := h.Token(t, admin)

	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+projectID.String()+"/comments", adminToken,
		map[string]any{"body": "Клиент тянет с оплатой, готовим напоминание", "is_internal": true})
	if code != http.StatusCreated {
		t.Fatalf("внутренний комментарий: код %d, тело %v", code, body)
	}
	code, body = h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+projectID.String()+"/comments", adminToken,
		map[string]any{"body": "Смонтировали первый ролик, ждём вашей правки", "is_internal": false})
	if code != http.StatusCreated {
		t.Fatalf("публичный комментарий: код %d, тело %v", code, body)
	}

	// Менеджерская выдача — оба.
	code, body = h.Do(t, http.MethodGet,
		"/api/v1/manager/projects/"+projectID.String()+"/comments", adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("manager comments: код %d", code)
	}
	if items, _ := body["items"].([]any); len(items) != 2 {
		t.Fatalf("менеджер видит %d комментариев, ожидалось 2", len(items))
	}

	// Клиентская — только публичный.
	code, body = h.Do(t, http.MethodGet,
		"/api/v1/me/projects/"+projectID.String()+"/comments", h.Token(t, clientID), nil)
	if code != http.StatusOK {
		t.Fatalf("client comments: код %d, тело %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("клиент видит %d комментариев, ожидался 1", len(items))
	}
	only, _ := items[0].(map[string]any)
	if internal, _ := only["is_internal"].(bool); internal {
		t.Error("внутренний комментарий доехал до клиента")
	}
}

// Валидация менеджерских ручек: пустой заголовок проекта — это 400 с
// объяснением, а не 500 и не молча созданный безымянный проект.
func TestHarnessManagerCreateProjectValidation(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	manager, cleanup := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanup()
	token := h.Token(t, manager)

	// Воронка нужна настоящая: хендлер проверяет pipeline_id раньше
	// заголовка и клиента, и без него тест не доходит до той валидации,
	// ради которой написан.
	_, pipelineID, _, cleanupProject := setupPipelineAndProject(t, pool)
	defer cleanupProject()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"без заголовка", map[string]any{
			"pipeline_id": pipelineID.String(), "client_name": "Иван", "client_contact": "@ivan"}},
		{"без клиента", map[string]any{
			"pipeline_id": pipelineID.String(), "title": "Проект без заказчика"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.Do(t, http.MethodPost, "/api/v1/manager/projects", token, c.body)
			if code != http.StatusBadRequest {
				t.Fatalf("код %d, ожидался 400. тело: %v", code, body)
			}
			if got, _ := body["error"].(string); got != "invalid_input" {
				t.Errorf("код ошибки %q, ожидался invalid_input", got)
			}
			if msg, _ := body["message"].(string); msg == "" {
				t.Error("валидация без объяснения — менеджер не поймёт, что править")
			}
		})
	}
}

// Несуществующий проект в менеджерской ручке — 404, а не 500.
func TestHarnessManagerUnknownProject(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	manager, cleanup := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanup()

	code, body := h.Do(t, http.MethodGet,
		"/api/v1/manager/projects/"+uuid.NewString(), h.Token(t, manager), nil)
	if code != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404. тело: %v", code, body)
	}
}

// Отчёт заказчику: доступен, только если проект его И показ статистики
// включён. На «не ваш проект» и «цифры закрыты» ответ одинаковый —
// иначе перебором можно узнать, какие проекты существуют.
func TestHarnessClientReportGating(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	var clientID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, projectID).Scan(&clientID); err != nil {
		t.Fatalf("read client: %v", err)
	}
	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupStranger()

	path := "/api/v1/me/projects/" + projectID.String() + "/report"

	// По умолчанию client_sees_stats = TRUE.
	code, body := h.Do(t, http.MethodGet, path, h.Token(t, clientID), nil)
	if code != http.StatusOK {
		t.Fatalf("заказчик своего проекта: код %d, тело %v", code, body)
	}

	code, _ = h.Do(t, http.MethodGet, path, h.Token(t, stranger), nil)
	if code != http.StatusNotFound {
		t.Errorf("чужой проект: код %d, ожидался 404", code)
	}

	// Менеджер закрыл статистику.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET client_sees_stats = FALSE WHERE id = $1`, projectID); err != nil {
		t.Fatalf("close stats: %v", err)
	}
	code, body = h.Do(t, http.MethodGet, path, h.Token(t, clientID), nil)
	if code != http.StatusNotFound {
		t.Errorf("закрытая статистика: код %d, ожидался 404", code)
	}
	if got, _ := body["error"].(string); got != "not_found" {
		t.Errorf("код ошибки %q — он должен совпадать с «чужой проект», иначе перебором узнают состав", got)
	}

	// Креатору проекта отчёт доступен всегда — но только по своим роликам.
	code, _ = h.Do(t, http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID.String()+"/report", h.Token(t, creators[0]), nil)
	if code != http.StatusOK {
		t.Errorf("креатор проекта: код %d, ожидался 200", code)
	}
	code, _ = h.Do(t, http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID.String()+"/report", h.Token(t, stranger), nil)
	if code != http.StatusNotFound {
		t.Errorf("посторонний в отчёте креатора: код %d, ожидался 404", code)
	}
}

// Выгрузка в CSV: с BOM, иначе Excel показывает кириллицу кракозябрами
// ровно тем, кому файл и нужен.
func TestHarnessReportCSVExport(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@u/video/1"}, map[int]int64{0: 1234})

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	rec := h.DoRaw(t, http.MethodGet,
		"/api/v1/manager/projects/"+projectID.String()+"/report.csv", h.Token(t, manager))

	if rec.Code != http.StatusOK {
		t.Fatalf("CSV: код %d, тело %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type %q, ожидался text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition %q — файл должен скачиваться, а не открываться", cd)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Error("нет BOM — Excel откроет кириллицу кракозябрами")
	}
	if !strings.Contains(body, "просмотры") {
		t.Error("в выгрузке нет заголовка колонок")
	}
	if !strings.Contains(body, "1234") {
		t.Error("в выгрузке нет цифр")
	}
}

// Менеджер не должен получать чужие проекты. Роль пускает его в /manager/*,
// но проект, назначенный коллеге, для него не существует — так же, как в
// остальной CRM.
func TestHarnessManagerCannotTouchForeignProject(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	owner, cleanupOwner := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupOwner()
	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupStranger()
	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	defer cleanupAdmin()

	// Проект назначен первому менеджеру.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET assigned_to_user_id = $2 WHERE id = $1`, projectID, owner); err != nil {
		t.Fatalf("assign project: %v", err)
	}

	base := "/api/v1/manager/projects/" + projectID.String()
	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"отчёт", http.MethodGet, base + "/report", nil},
		{"список выкладок", http.MethodGet, base + "/publications", nil},
		{"добавить креатора", http.MethodPost, base + "/creators",
			map[string]any{"creator_user_id": creators[0].String()}},
		{"снимок чеклиста", http.MethodPost, base + "/checklist",
			map[string]any{"template_id": uuid.NewString()}},
		{"даты пачкой", http.MethodPost, base + "/publications/batch",
			map[string]any{"creator_user_ids": []string{creators[0].String()},
				"scheme": "daily", "from": "2026-09-01", "to": "2026-09-02"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.Do(t, c.method, c.path, h.Token(t, stranger), c.body)
			if code != http.StatusNotFound {
				t.Errorf("чужой менеджер: код %d, ожидался 404. тело: %v", code, body)
			}
		})
	}

	// Свой менеджер и админ проходят.
	if code, body := h.Do(t, http.MethodGet, base+"/report", h.Token(t, owner), nil); code != http.StatusOK {
		t.Errorf("свой менеджер: код %d, тело %v", code, body)
	}
	if code, _ := h.Do(t, http.MethodGet, base+"/report", h.Token(t, admin), nil); code != http.StatusOK {
		t.Errorf("админ должен ходить менеджерскими URL, код %d", code)
	}
}

// Закрытая статистика должна быть закрыта во ВСЕХ ручках, отдающих цифры,
// а не только в отчёте. Через ленту роликов она утекала.
func TestHarnessClientFeedRespectsStatsSetting(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@u/video/1"}, map[int]int64{0: 4242})

	var clientID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, projectID).Scan(&clientID); err != nil {
		t.Fatalf("read client: %v", err)
	}
	path := "/api/v1/me/projects/" + projectID.String() + "/videos"

	// Статистика открыта — цифры видны.
	code, body := h.Do(t, http.MethodGet, path, h.Token(t, clientID), nil)
	if code != http.StatusOK {
		t.Fatalf("лента: код %d, тело %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("роликов в ленте %d, ожидался 1", len(items))
	}
	first, _ := items[0].(map[string]any)
	if views, _ := first["views"].(float64); views != 4242 {
		t.Errorf("просмотры %v, ожидалось 4242", first["views"])
	}

	// Менеджер закрыл статистику — лента остаётся, цифры уходят.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET client_sees_stats = FALSE WHERE id = $1`, projectID); err != nil {
		t.Fatalf("close stats: %v", err)
	}
	code, body = h.Do(t, http.MethodGet, path, h.Token(t, clientID), nil)
	if code != http.StatusOK {
		t.Fatalf("лента при закрытой статистике: код %d", code)
	}
	items, _ = body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("лента должна остаться: роликов %d", len(items))
	}
	first, _ = items[0].(map[string]any)
	if views, _ := first["views"].(float64); views != 0 {
		t.Errorf("цифры утекли через ленту: просмотры %v", first["views"])
	}
	if hidden, _ := first["stats_hidden"].(bool); !hidden {
		t.Error("нет отметки stats_hidden — ноль будет прочитан как «никто не смотрел»")
	}
}

// Чеклист проекта — не публичный документ: он отдаётся только участникам.
func TestHarnessChecklistRequiresMembership(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	outsider, cleanupOutsider := h.NewUser(t, userOpts{Kind: "specialist"})
	defer cleanupOutsider()

	path := "/api/v1/me/creator/projects/" + projectID.String() + "/checklist"

	if code, _ := h.Do(t, http.MethodGet, path, h.Token(t, creators[0]), nil); code != http.StatusOK {
		t.Errorf("участник проекта: код %d, ожидался 200", code)
	}
	if code, body := h.Do(t, http.MethodGet, path, h.Token(t, outsider), nil); code != http.StatusNotFound {
		t.Errorf("посторонний получил чеклист: код %d, тело %v", code, body)
	}
}

// Креатор видит разбивку просмотров по площадкам — но только своих
// роликов. Чужие цифры в его отчёт не попадают: креаторов в проекте
// бывает несколько, и чужая статистика — не его дело.
func TestHarnessCreatorReportOwnPlatforms(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	if len(creators) < 2 {
		t.Skip("нужны двое креаторов")
	}
	seedStats(t, projectID, creators[0], []string{
		"https://www.tiktok.com/@u/video/1",
		"https://www.youtube.com/shorts/abc",
	}, map[int]int64{0: 1234})
	seedStats(t, projectID, creators[1], []string{
		"https://www.tiktok.com/@other/video/2",
	}, map[int]int64{0: 9999})

	code, cr := h.Do(t, http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID.String()+"/report", h.Token(t, creators[0]), nil)
	if code != http.StatusOK {
		t.Fatalf("отчёт креатора: код %d, тело %v", code, cr)
	}
	if plats, _ := cr["by_platform"].([]any); len(plats) == 0 {
		t.Errorf("креатору не пришёл разрез по площадкам: %v", cr["by_platform"])
	}
	rows, _ := cr["videos_table"].([]any)
	if len(rows) != 2 {
		t.Fatalf("у креатора строк ссылок %d, ожидалось 2 — только его собственные", len(rows))
	}
	for _, r := range rows {
		row := r.(map[string]any)
		if _, ok := row["views"]; !ok {
			t.Errorf("в строке ссылки креатора нет просмотров: %v", row)
		}
		if row["url"] == "https://www.tiktok.com/@other/video/2" {
			t.Errorf("креатору пришёл чужой ролик: %v", row)
		}
	}
}
