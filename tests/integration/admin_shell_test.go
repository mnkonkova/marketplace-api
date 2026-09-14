package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/audit"
	"marketpclce/internal/auth"
	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Оболочка админки: сводка, карточка человека, журнал, команда, поиск.
//
// Всё через HTTP: половина поведения живёт в разборе параметров и в
// проверке роли админа, и тест на repo прошёл бы мимо неё.
//
// Тестовая база общая, поэтому абсолютные числа в сводке и журнале
// проверять нельзя — соседние тесты оставляют в ней свои строки. Там,
// где важно число, сравниваем «до» и «после»: дельта принадлежит только
// этому тесту.

// adminShell — админ, его токен и уборка за тестом.
type adminShell struct {
	h        *integration.APIHarness
	token    string
	adminID  uuid.UUID
	cleanups []func()
}

func newAdminShell(t *testing.T, pool *pgxpool.Pool) *adminShell {
	t.Helper()
	h := newAPIHarness(t, pool)
	adminID, cleanup := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	s := &adminShell{h: h, token: h.Token(t, adminID), adminID: adminID}
	s.cleanups = append(s.cleanups, cleanup)
	t.Cleanup(s.cleanup)
	return s
}

func (s *adminShell) cleanup() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

func (s *adminShell) defer_(f func()) { s.cleanups = append(s.cleanups, f) }

// user — пользователь с уборкой, привязанной к тесту.
func (s *adminShell) user(t *testing.T, o userOpts) uuid.UUID {
	t.Helper()
	id, cleanup := s.h.NewUser(t, o)
	s.defer_(cleanup)
	return id
}

// project — проект вида «креаторы под ключ»: воронки у него нет, и ради
// проверки списка её поднимать не приходится.
func (s *adminShell) project(t *testing.T, in projects.StartProjectInput) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if in.Kind == "" {
		in.Kind = projects.KindCreatorsTurnkey
	}
	in.Source = projects.SourceManual
	id, err := projects.NewRepo(s.h.Pool).StartProject(ctx, in)
	if err != nil {
		t.Fatalf("создать проект %q: %v", in.Title, err)
	}
	s.defer_(func() {
		_, _ = s.h.Pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'project' AND object_id = $1`, id.String())
		_, _ = s.h.Pool.Exec(ctx,
			`DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, id.String())
		_, _ = s.h.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

func (s *adminShell) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	return s.h.Do(t, http.MethodGet, path, s.token, nil)
}

func (s *adminShell) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	return s.h.Do(t, http.MethodPost, path, s.token, body)
}

// ---- мелкие помощники по JSON ----

func subMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет объекта %q: %v", key, m)
	}
	return v
}

func num(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("в ответе нет числа %q: %v", key, m)
	}
	return int(v)
}

func list(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("в ответе нет списка %q: %v", key, m)
	}
	return v
}

// Сводка отдаёт все пункты всегда — даже те, где ничего не висит.
// Пункт, исчезающий при нуле, фронт не отличит от пункта, который сервер
// забыл посчитать: в первом случае надо нарисовать «чисто», во втором —
// не рисовать ничего.
func TestAdminSummaryAlwaysReturnsEveryBlock(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)

	code, body := s.get(t, "/api/v1/admin/summary")
	if code != http.StatusOK {
		t.Fatalf("сводка: код %d, тело %v", code, body)
	}
	attention := subMap(t, body, "attention")
	for _, key := range []string{
		"moderation", "projects_unassigned", "projects_stale",
		"managers_unapproved", "specialist_not_confirmed",
		"publications_overdue", "work_without_prepayment", "revisions_exceeded",
	} {
		block := subMap(t, attention, key)
		if _, ok := block["count"].(float64); !ok {
			t.Errorf("пункт %q без числа: %v", key, block)
		}
		if _, ok := block["items"].([]any); !ok {
			t.Errorf("пункт %q без списка items: %v", key, block)
		}
	}
	if _, ok := subMap(t, attention, "moderation")["over_day"].(float64); !ok {
		t.Error("модерация без over_day — непонятно, нарушен ли суточный срок")
	}

	// Распределения: ключи на месте даже там, где проектов нет вовсе.
	byKind := subMap(t, body, "projects_by_kind")
	for _, k := range []string{"creators_turnkey", "production_turnkey", "general"} {
		if _, ok := byKind[k].(float64); !ok {
			t.Errorf("в распределении по видам нет %q: %v", k, byKind)
		}
	}
	byStatus := subMap(t, body, "projects_by_status")
	for _, st := range []string{"draft", "active", "on_hold", "done", "cancelled", "dispute"} {
		if _, ok := byStatus[st].(float64); !ok {
			t.Errorf("в распределении по статусам нет %q: %v", st, byStatus)
		}
	}
	if _, ok := body["managers"].([]any); !ok {
		t.Errorf("в сводке нет нагрузки менеджеров: %v", body)
	}
}

// Пункты сводки считают то, что обещают. Абсолютные числа в общей базе
// ничего не значат, поэтому сравниваем «до» и «после»: каждый заведённый
// здесь проект обязан сдвинуть свой пункт ровно на единицу.
func TestAdminSummaryCountsWhatItPromises(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	before := summaryCounts(t, s)

	client := s.user(t, userOpts{Kind: "client"})
	// Без ответственного — в «проекты без менеджера».
	s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 без менеджера",
	})
	// Правок больше, чем включено в условия.
	over := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 перебор правок",
	})
	manager := s.user(t, userOpts{Kind: "client", IsManager: true})
	if _, err := pool.Exec(ctx, `
UPDATE projects SET revisions_used = revisions_included + 1,
                    assigned_to_user_id = $2
WHERE id = $1`, over, manager); err != nil {
		t.Fatalf("перебор правок: %v", err)
	}
	// Менеджер без доступа.
	s.user(t, userOpts{Kind: "client", IsManager: true, NotApprove: true})

	after := summaryCounts(t, s)
	for _, c := range []struct {
		block string
		want  int
	}{
		{"projects_unassigned", 1},
		{"revisions_exceeded", 1},
		{"managers_unapproved", 1},
	} {
		if got := after[c.block] - before[c.block]; got != c.want {
			t.Errorf("пункт %q: прирост %d, ожидали %d", c.block, got, c.want)
		}
	}
}

// Счётчики меню сходятся с тем, что реально отдают соответствующие
// списки, и тестовые записи не попадают ни в один из них.
//
// Абсолютные значения в общей базе ничего не значат, поэтому заводим по
// объекту каждого вида и сверяем прирост.
func TestAdminSummaryNavCounts(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	before := navCounts(t, s)

	client := s.user(t, userOpts{Kind: "client"})
	specialist := s.user(t, userOpts{Kind: "specialist"})
	// Менеджер идёт и в «команду», и в «клиентов»: харнесс заводит его
	// с kind=client, роль живёт отдельным флагом.
	s.user(t, userOpts{Kind: "client", IsManager: true})
	s.project(t, projects.StartProjectInput{ClientUserID: &client, Title: "R2 меню активный"})

	// Специалист в очереди модерации — коралловый бейдж.
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, is_published, moderation_status)
VALUES ($1, 'R2 на модерации', TRUE, 'pending_review')
ON CONFLICT (user_id) DO UPDATE SET is_published = TRUE, moderation_status = 'pending_review'`,
		specialist); err != nil {
		t.Fatalf("профиль на модерации: %v", err)
	}

	name := "R2 меню " + uuid.NewString()[:8]
	code, tpl := s.post(t, "/api/v1/admin/checklist_templates", map[string]any{
		"name":  name,
		"items": []map[string]any{{"text": "Пункт", "is_required": true}},
	})
	if code != http.StatusCreated {
		t.Fatalf("шаблон чеклиста: код %d, тело %v", code, tpl)
	}
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tpl["id"])
	})

	var pipelineID, productionID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO pipelines (name) VALUES ($1) RETURNING id`, name+" воронка").Scan(&pipelineID); err != nil {
		t.Fatalf("воронка: %v", err)
	}
	s.defer_(func() { _, _ = pool.Exec(ctx, `DELETE FROM pipelines WHERE id = $1`, pipelineID) })
	if err := pool.QueryRow(ctx,
		`INSERT INTO productions (name) VALUES ($1) RETURNING id`, name+" продакшен").Scan(&productionID); err != nil {
		t.Fatalf("продакшен: %v", err)
	}
	s.defer_(func() { _, _ = pool.Exec(ctx, `DELETE FROM productions WHERE id = $1`, productionID) })

	after := navCounts(t, s)
	for _, c := range []struct {
		key  string
		want int
	}{
		{"projects_active", 1},
		{"moderation_pending", 1},
		{"team", 1}, // менеджер; админ теста завёлся до первого замера
		{"specialists", 1},
		{"clients", 2}, // заказчик и менеджер: менеджер заведён с kind=client
		{"checklists", 1},
		{"pipelines", 1},
		{"productions", 1},
	} {
		if got := after[c.key] - before[c.key]; got != c.want {
			t.Errorf("счётчик %q: прирост %d, ожидали %d", c.key, got, c.want)
		}
	}

	// Числа сходятся со списками, которые откроют по клику.
	_, body := s.get(t, "/api/v1/admin/team")
	if got := len(list(t, body, "items")); got != after["team"] {
		t.Errorf("в счётчике команды %d, а в списке %d", after["team"], got)
	}
	_, body = s.get(t, "/api/v1/admin/users?kind=specialist&limit=1")
	if got := num(t, body, "total"); got != after["specialists"] {
		t.Errorf("в счётчике специалистов %d, а в списке %d", after["specialists"], got)
	}
	_, body = s.get(t, "/api/v1/admin/users?kind=client&limit=1")
	if got := num(t, body, "total"); got != after["clients"] {
		t.Errorf("в счётчике клиентов %d, а в списке %d", after["clients"], got)
	}
	_, body = s.get(t, "/api/v1/admin/checklist_templates")
	if got := len(list(t, body, "items")); got != after["checklists"] {
		t.Errorf("в счётчике чеклистов %d, а в библиотеке %d", after["checklists"], got)
	}

	// Тестовые не попадают никуда: ни проект, ни человек.
	s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 меню тестовый", IsTest: true,
	})
	s.user(t, userOpts{Kind: "specialist", IsTest: true})
	s.user(t, userOpts{Kind: "client", IsManager: true, IsTest: true})
	withTest := navCounts(t, s)
	for _, key := range []string{"projects_active", "team", "specialists", "clients"} {
		if withTest[key] != after[key] {
			t.Errorf("тестовые записи попали в счётчик %q: было %d, стало %d",
				key, after[key], withTest[key])
		}
	}

	// Завершённый проект уходит из «в работе»: цифра у пункта меню
	// отвечает на вопрос «сколько в работе», а не «сколько строк».
	donePID := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 меню завершённый",
	})
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET status = 'done', completed_at = now() WHERE id = $1`, donePID); err != nil {
		t.Fatalf("завершить проект: %v", err)
	}
	if got := navCounts(t, s)["projects_active"]; got != after["projects_active"] {
		t.Errorf("завершённый проект попал в «в работе»: было %d, стало %d",
			after["projects_active"], got)
	}

	// Выключенная воронка и деактивированный продакшен — архив, в меню
	// им делать нечего.
	if _, err := pool.Exec(ctx, `UPDATE pipelines SET is_active = FALSE WHERE id = $1`, pipelineID); err != nil {
		t.Fatalf("выключить воронку: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE productions SET is_active = FALSE WHERE id = $1`, productionID); err != nil {
		t.Fatalf("выключить продакшен: %v", err)
	}
	archived := navCounts(t, s)
	if archived["pipelines"] != before["pipelines"] {
		t.Errorf("выключенная воронка осталась в счётчике: %d против %d",
			archived["pipelines"], before["pipelines"])
	}
	if archived["productions"] != before["productions"] {
		t.Errorf("выключенный продакшен остался в счётчике: %d против %d",
			archived["productions"], before["productions"])
	}
}

// navCounts — блок счётчиков меню из сводки.
func navCounts(t *testing.T, s *adminShell) map[string]int {
	t.Helper()
	code, body := s.get(t, "/api/v1/admin/summary")
	if code != http.StatusOK {
		t.Fatalf("сводка: код %d, тело %v", code, body)
	}
	block := subMap(t, body, "nav_counts")
	out := map[string]int{}
	for _, key := range []string{
		"projects_active", "moderation_pending", "team",
		"specialists", "clients", "checklists", "pipelines", "productions",
	} {
		out[key] = num(t, block, key)
	}
	return out
}

func summaryCounts(t *testing.T, s *adminShell) map[string]int {
	t.Helper()
	code, body := s.get(t, "/api/v1/admin/summary")
	if code != http.StatusOK {
		t.Fatalf("сводка: код %d, тело %v", code, body)
	}
	attention := subMap(t, body, "attention")
	out := map[string]int{}
	for key, raw := range attention {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out[key] = num(t, block, "count")
	}
	return out
}

// Карточка человека собирает то, что раньше искали в четырёх местах:
// профиль, проекты во всех ролях и журнал по нему.
func TestAdminUserCardCollectsEverything(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	client := s.user(t, userOpts{Kind: "client"})
	if _, err := pool.Exec(ctx,
		`UPDATE users SET email_verified_at = NULL WHERE id = $1`, client); err != nil {
		t.Fatalf("сбросить подтверждение почты: %v", err)
	}
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 карточка клиента",
	})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, client.String())
	})

	code, _ := s.post(t, "/api/v1/admin/users/"+client.String()+"/verify_email", nil)
	if code != http.StatusNoContent {
		t.Fatalf("подтверждение почты: код %d", code)
	}

	code, body := s.get(t, "/api/v1/admin/users/"+client.String())
	if code != http.StatusOK {
		t.Fatalf("карточка: код %d, тело %v", code, body)
	}
	if v, _ := body["email_verified"].(bool); !v {
		t.Error("почта подтверждена, а в карточке нет")
	}
	if _, ok := body["last_login_at"]; ok {
		t.Error("человек не входил ни разу — поля last_login_at быть не должно")
	}

	found := false
	for _, raw := range list(t, body, "projects") {
		p, _ := raw.(map[string]any)
		if p["id"] == pid.String() {
			found = true
			if p["role"] != "client" {
				t.Errorf("роль в проекте %v, ожидали client", p["role"])
			}
		}
	}
	if !found {
		t.Errorf("проект клиента не попал в карточку: %v", body["projects"])
	}

	entries := list(t, body, "audit")
	if !hasAction(entries, audit.ActionUserVerifyEmail) {
		t.Errorf("в журнале карточки нет подтверждения почты: %v", entries)
	}
}

// В карточке специалиста — все пять площадок, включая незаполненные:
// блок называется «Площадки · 3 из 5», и пустые строки в нём и есть
// ответ на вопрос «чего не хватает».
func TestAdminUserCardShowsAllFivePlatforms(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	specialist := s.user(t, userOpts{Kind: "specialist"})
	// Ники живут там же, где их правит сам специалист, — в social_links
	// профиля. Второго места для них нет.
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, social_links)
VALUES ($1, 'R2 площадки', $2::jsonb)
ON CONFLICT (user_id) DO UPDATE SET social_links = EXCLUDED.social_links`,
		specialist, `{"tiktok":"@r2tt","youtube":"https://youtube.com/@r2","telegram":"@r2tg"}`); err != nil {
		t.Fatalf("профиль специалиста: %v", err)
	}

	_, body := s.get(t, "/api/v1/admin/users/"+specialist.String())
	items := list(t, body, "platforms")
	if len(items) != 5 {
		t.Fatalf("площадок в карточке %d, ожидали 5: %v", len(items), items)
	}
	got := map[string]string{}
	order := make([]string, 0, 5)
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		name, _ := m["platform"].(string)
		handle, _ := m["handle"].(string)
		got[name] = handle
		order = append(order, name)
	}
	if got["tiktok"] != "@r2tt" || got["youtube"] != "https://youtube.com/@r2" {
		t.Errorf("заполненные площадки приехали не те: %v", got)
	}
	for _, empty := range []string{"instagram", "vk", "likee"} {
		v, ok := got[empty]
		if !ok {
			t.Errorf("площадки %q нет в карточке — фронту нечем показать «не заполнено»", empty)
		}
		if v != "" {
			t.Errorf("площадка %q неожиданно заполнена: %q", empty, v)
		}
	}
	// Телеграм — контакт, а не площадка выкладки: в блоке ему не место.
	if _, ok := got["telegram"]; ok {
		t.Error("в площадки затесался телеграм из контактов")
	}
	// Порядок фиксирован: он же порядок колонок в интерфейсе.
	want := []string{"tiktok", "instagram", "youtube", "vk", "likee"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("порядок площадок %v, ожидали %v", order, want)
			break
		}
	}

	// У клиента профиля специалиста нет — и площадок не бывает: блок
	// фронт не нарисует.
	client := s.user(t, userOpts{Kind: "client"})
	_, body = s.get(t, "/api/v1/admin/users/"+client.String())
	if raw, ok := body["platforms"]; ok {
		t.Errorf("у клиента приехали площадки: %v", raw)
	}
}

func hasAction(entries []any, action string) bool {
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		if e["action"] == action {
			return true
		}
	}
	return false
}

// Журнал пишется в транзакции действия. Если действие не состоялось,
// записи быть не должно — иначе история врёт ровно там, где к ней
// обращаются: «кто это сделал» при том, что никто ничего не делал.
func TestAuditEntryDisappearsWithFailedAction(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	// Действие удалось — запись есть.
	client := s.user(t, userOpts{Kind: "client"})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, client.String())
	})
	if code, _ := s.post(t, "/api/v1/admin/users/"+client.String()+"/deactivate", nil); code != http.StatusNoContent {
		t.Fatalf("деактивация: код %d", code)
	}
	if n := auditCount(t, pool, audit.ObjectUser, client.String()); n != 1 {
		t.Fatalf("после удавшегося действия записей %d, ожидали 1", n)
	}

	// Действия не было — записи тоже нет.
	ghost := uuid.New()
	code, _ := s.post(t, "/api/v1/admin/users/"+ghost.String()+"/verify_email", nil)
	if code != http.StatusNotFound {
		t.Fatalf("подтверждение почты несуществующему: код %d, ожидали 404", code)
	}
	if n := auditCount(t, pool, audit.ObjectUser, ghost.String()); n != 0 {
		t.Errorf("действие не состоялось, а записей в журнале %d", n)
	}

	// И то же самое на уровне самой записи: она живёт в чужой транзакции
	// и обязана исчезнуть вместе с ней. Проверяем прямо, потому что
	// через HTTP откат после записи не воспроизвести — в доменах журнал
	// пишется последним шагом.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := audit.Write(ctx, tx, s.adminID, audit.ActionUserMarkTest,
		audit.ObjectUser, ghost.String(), map[string]any{"is_test": true}); err != nil {
		t.Fatalf("audit.Write: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := auditCount(t, pool, audit.ObjectUser, ghost.String()); n != 0 {
		t.Errorf("транзакция откатилась, а запись в журнале осталась (%d)", n)
	}
}

func auditCount(t *testing.T, pool *pgxpool.Pool, objectType, objectID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM admin_audit_log WHERE object_type = $1 AND object_id = $2`,
		objectType, objectID).Scan(&n); err != nil {
		t.Fatalf("считать журнал: %v", err)
	}
	return n
}

// Журнал читается с фильтрами: «что делали с этим объектом» — тот
// вопрос, ради которого он и нужен.
func TestAdminAuditFilters(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	target := s.user(t, userOpts{Kind: "client"})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, target.String())
	})
	if code, _ := s.post(t, "/api/v1/admin/users/"+target.String()+"/mark_test",
		map[string]any{"is_test": true}); code != http.StatusNoContent {
		t.Fatal("не удалось пометить тестовым")
	}

	code, body := s.get(t, "/api/v1/admin/audit?object_type=user&object_id="+target.String())
	if code != http.StatusOK {
		t.Fatalf("журнал: код %d, тело %v", code, body)
	}
	if got := num(t, body, "total"); got != 1 {
		t.Fatalf("записей по объекту %d, ожидали 1: %v", got, body)
	}
	entry, _ := list(t, body, "items")[0].(map[string]any)
	if entry["action"] != audit.ActionUserMarkTest {
		t.Errorf("действие %v, ожидали %s", entry["action"], audit.ActionUserMarkTest)
	}
	if entry["actor_user_id"] != s.adminID.String() {
		t.Errorf("актор %v, ожидали %s", entry["actor_user_id"], s.adminID)
	}
	if payload, ok := entry["payload"].(map[string]any); !ok || payload["is_test"] != true {
		t.Errorf("в payload нет того, что поменяли: %v", entry["payload"])
	}

	// Фильтр по действию, которого у объекта не было, отдаёт пусто, а не
	// «все записи».
	_, body = s.get(t, "/api/v1/admin/audit?object_id="+target.String()+"&action="+audit.ActionProjectRestore)
	if got := num(t, body, "total"); got != 0 {
		t.Errorf("по чужому действию нашлось %d записей", got)
	}
}

// Список команды показывает нагрузку и последний вход. NULL в
// last_login_at означает «не входил ни разу» — сотрудника, который так и
// не зашёл, надо позвать, а не ждать.
func TestAdminTeamShowsLoadAndLastLogin(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	manager := s.user(t, userOpts{Kind: "client", IsManager: true})
	// Просроченный проект: срок прошёл, статус рабочий.
	pid := s.project(t, projects.StartProjectInput{
		ClientName: "Заказчик без аккаунта", ClientContact: "@tg",
		AssignedToUserID: &manager, Title: "R2 просрочка",
	})
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET status = 'active', due_date = CURRENT_DATE - 3 WHERE id = $1`, pid); err != nil {
		t.Fatalf("состарить срок: %v", err)
	}

	row := teamRow(t, s, manager)
	if got := num(t, row, "active_projects"); got != 1 {
		t.Errorf("активных проектов %d, ожидали 1", got)
	}
	if got := num(t, row, "overdue_projects"); got != 1 {
		t.Errorf("просроченных проектов %d, ожидали 1", got)
	}
	if _, ok := row["last_login_at"]; ok {
		t.Error("менеджер не входил — last_login_at не должен приходить")
	}

	// Обновление пары токенов — тоже вход: сессия живёт неделями, и без
	// этого «последний вход» показывал бы дату ввода пароля.
	issuer := auth.NewTokenIssuer("harness-secret", 15*time.Minute, 7*24*time.Hour)
	pair, err := issuer.Issue(manager, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := auth.NewService(auth.NewRepo(pool), issuer).Refresh(ctx, pair.Refresh); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	row = teamRow(t, s, manager)
	if _, ok := row["last_login_at"]; !ok {
		t.Error("после обновления токенов last_login_at не проставился")
	}
}

func teamRow(t *testing.T, s *adminShell, userID uuid.UUID) map[string]any {
	t.Helper()
	code, body := s.get(t, "/api/v1/admin/team")
	if code != http.StatusOK {
		t.Fatalf("команда: код %d, тело %v", code, body)
	}
	for _, raw := range list(t, body, "items") {
		m, _ := raw.(map[string]any)
		if m["user_id"] == userID.String() {
			return m
		}
	}
	t.Fatalf("менеджера нет в списке команды: %v", body)
	return nil
}

// Тестовые записи по умолчанию скрыты: их после прогонов на стенде
// больше, чем настоящих, и в списке ищут не их.
func TestAdminHidesTestProjectsAndUsers(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	marker := "r2test" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	client := s.user(t, userOpts{Kind: "client"})
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: marker + " проект",
	})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, client.String())
	})

	if code, _ := s.post(t, "/api/v1/admin/projects/"+pid.String()+"/mark_test",
		map[string]any{"is_test": true}); code != http.StatusNoContent {
		t.Fatal("не удалось пометить проект тестовым")
	}

	_, body := s.get(t, "/api/v1/admin/projects?q="+marker)
	if got := num(t, body, "total"); got != 0 {
		t.Errorf("тестовый проект виден в списке по умолчанию (%d)", got)
	}
	_, body = s.get(t, "/api/v1/admin/projects?include_test=true&q="+marker)
	if got := num(t, body, "total"); got != 1 {
		t.Errorf("с include_test=true тестовый проект не нашёлся (%d)", got)
	}
	// Быстрый поиск тестовые тоже не показывает: подсказка — место для
	// настоящих строк.
	_, body = s.get(t, "/api/v1/admin/search?q="+marker)
	if n := len(list(t, body, "projects")); n != 0 {
		t.Errorf("тестовый проект попал в ⌘K-поиск (%d)", n)
	}

	// Снятая пометка возвращает проект в список.
	if code, _ := s.post(t, "/api/v1/admin/projects/"+pid.String()+"/mark_test",
		map[string]any{"is_test": false}); code != http.StatusNoContent {
		t.Fatal("не удалось снять пометку")
	}
	_, body = s.get(t, "/api/v1/admin/projects?q="+marker)
	if got := num(t, body, "total"); got != 1 {
		t.Errorf("после снятия пометки проект не вернулся в список (%d)", got)
	}

	// То же самое с людьми.
	email := userEmail(t, pool, client)
	if code, _ := s.post(t, "/api/v1/admin/users/"+client.String()+"/mark_test",
		map[string]any{"is_test": true}); code != http.StatusNoContent {
		t.Fatal("не удалось пометить пользователя тестовым")
	}
	_, body = s.get(t, "/api/v1/admin/users?q="+email)
	if got := num(t, body, "total"); got != 0 {
		t.Errorf("тестовый пользователь виден в списке по умолчанию (%d)", got)
	}
	_, body = s.get(t, "/api/v1/admin/users?include_test=true&q="+email)
	if got := num(t, body, "total"); got != 1 {
		t.Errorf("с include_test=true тестовый пользователь не нашёлся (%d)", got)
	}

	// Админа пометить тестовым нельзя: он пропал бы из списка команды —
	// и первым делом из списка тех, кто может это исправить.
	code, _ := s.post(t, "/api/v1/admin/users/"+s.adminID.String()+"/mark_test",
		map[string]any{"is_test": true})
	if code != http.StatusBadRequest {
		t.Errorf("пометка админа тестовым: код %d, ожидали 400", code)
	}
}

func userEmail(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var email string
	if err := pool.QueryRow(context.Background(),
		`SELECT email::text FROM users WHERE id = $1`, id).Scan(&email); err != nil {
		t.Fatalf("почта пользователя: %v", err)
	}
	return email
}

// ⌘K-поиск отвечает одной строкой и про проекты, и про людей: человека
// почти всегда ищут вместе с его проектом.
func TestAdminGlobalSearchFindsBothSides(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	marker := "r2find" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	client := s.user(t, userOpts{Kind: "client"})
	if _, err := pool.Exec(ctx, `
INSERT INTO client_profiles (user_id, display_name) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET display_name = EXCLUDED.display_name`,
		client, marker+" Заказчиков"); err != nil {
		t.Fatalf("профиль клиента: %v", err)
	}
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: marker + " монтаж",
	})

	code, body := s.get(t, "/api/v1/admin/search?q="+marker)
	if code != http.StatusOK {
		t.Fatalf("поиск: код %d, тело %v", code, body)
	}
	found := false
	for _, raw := range list(t, body, "projects") {
		if m, _ := raw.(map[string]any); m["id"] == pid.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("проект не нашёлся: %v", body["projects"])
	}
	found = false
	for _, raw := range list(t, body, "users") {
		if m, _ := raw.(map[string]any); m["id"] == client.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("клиент не нашёлся по имени в профиле: %v", body["users"])
	}

	// Одна буква — не запрос: по ней совпадёт всё, и подсказка станет шумом.
	_, body = s.get(t, "/api/v1/admin/search?q="+"я")
	if n := len(list(t, body, "projects")) + len(list(t, body, "users")); n != 0 {
		t.Errorf("поиск по одному символу что-то нашёл (%d)", n)
	}
}

// status=unfinished — «в работе»: ровно те четыре статуса, что считает
// цифра в меню. Ради этого значение и добавлено: список по клику должен
// показывать столько же строк, сколько обещал счётчик.
func TestAdminProjectsUnfinishedMatchesNavCount(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	marker := "r2unf" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	client := s.user(t, userOpts{Kind: "client"})

	// По проекту на каждый статус, включая те, что в «в работе» не входят.
	byStatus := map[string]uuid.UUID{}
	for _, st := range []string{"draft", "active", "on_hold", "dispute", "done", "cancelled"} {
		id := s.project(t, projects.StartProjectInput{
			ClientUserID: &client, Title: marker + " " + st,
		})
		// Статус выставляем всем, включая draft: StartProject сам решает,
		// с какого статуса начать проект, и полагаться на это здесь
		// значило бы проверять его, а не фильтр.
		if _, err := pool.Exec(ctx,
			`UPDATE projects SET status = $2::project_status WHERE id = $1`, id, st); err != nil {
			t.Fatalf("выставить статус %s: %v", st, err)
		}
		byStatus[st] = id
	}

	_, body := s.get(t, "/api/v1/admin/projects?status=unfinished&q="+marker+"&limit=100")
	got := map[string]bool{}
	for _, raw := range list(t, body, "items") {
		m, _ := raw.(map[string]any)
		st, _ := m["status"].(string)
		got[st] = true
	}
	for _, st := range []string{"draft", "active", "on_hold", "dispute"} {
		if !got[st] {
			t.Errorf("status=unfinished не отдал %q", st)
		}
	}
	for _, st := range []string{"done", "cancelled"} {
		if got[st] {
			t.Errorf("status=unfinished отдал %q — этот проект уже не в работе", st)
		}
	}
	if n := num(t, body, "total"); n != 4 {
		t.Errorf("total при status=unfinished %d, ожидали 4", n)
	}

	// Главное: список и счётчик считают одно и то же. Сверяем приростом —
	// в общей базе есть чужие проекты.
	beforeCount := navCounts(t, s)["projects_active"]
	s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: marker + " ещё в работе",
	})
	_, body = s.get(t, "/api/v1/admin/projects?status=unfinished&limit=1")
	afterList := num(t, body, "total")
	afterCount := navCounts(t, s)["projects_active"]
	if afterCount != beforeCount+1 {
		t.Fatalf("счётчик меню: было %d, стало %d", beforeCount, afterCount)
	}
	if afterList != afterCount {
		t.Errorf("в списке %d проектов в работе, а в меню %d — числа обязаны сходиться",
			afterList, afterCount)
	}

	// Пустой status не изменился: отдаёт завершённые, прячет отменённые.
	_, body = s.get(t, "/api/v1/admin/projects?q="+marker+"&limit=100")
	got = map[string]bool{}
	for _, raw := range list(t, body, "items") {
		m, _ := raw.(map[string]any)
		st, _ := m["status"].(string)
		got[st] = true
	}
	if !got["done"] {
		t.Error("пустой status перестал отдавать завершённые — сломали существующие вызовы")
	}
	if got["cancelled"] {
		t.Error("пустой status начал отдавать отменённые")
	}

	// Точное значение работает как работало.
	_, body = s.get(t, "/api/v1/admin/projects?status=done&q="+marker)
	if n := num(t, body, "total"); n != 1 {
		t.Errorf("status=done отдал %d проектов, ожидали 1", n)
	}
	_, body = s.get(t, "/api/v1/admin/projects?status=cancelled&q="+marker)
	if n := num(t, body, "total"); n != 1 {
		t.Errorf("status=cancelled отдал %d проектов, ожидали 1", n)
	}

	// Незнакомое значение — понятный отказ, а не «что-то пошло не так».
	// Фильтры живут в адресе и ссылками делятся: устаревшая ссылка или
	// опечатка в ней роняли экран пятисотой. Отказ обязан перечислить,
	// что принимается, — иначе с чужой ссылкой не догадаться, что чинить.
	code, body := s.get(t, "/api/v1/admin/projects?status=nonsense")
	if code != http.StatusBadRequest {
		t.Fatalf("незнакомый статус: код %d, ожидали 400 (тело %v)", code, body)
	}
	if body["error"] != "invalid_input" {
		t.Errorf("код ошибки %v, ожидали invalid_input", body["error"])
	}
	msg, _ := body["message"].(string)
	for _, want := range []string{"unfinished", "cancelled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в сообщении нет допустимого значения %q: %q", want, msg)
		}
	}
}

// Список проектов сузился по виду, а прогресс читается числом: «62%» не
// отвечает на вопрос, сколько роликов осталось выложить.
func TestAdminProjectsKindFilterAndProgressNumbers(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	marker := "r2kind" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	client := s.user(t, userOpts{Kind: "client"})
	creator := s.user(t, userOpts{Kind: "specialist"})
	creators := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: marker + " креаторы",
	})
	// Общий проект заводим его же ручкой: у него обязателен срок, и
	// StartProject такой строки не создаст (проверка в БД). Исполнителю
	// нужен опубликованный профиль — иначе проект ему не отдать.
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, is_published, moderation_status)
VALUES ($1, $2, TRUE, 'approved')
ON CONFLICT (user_id) DO UPDATE SET is_published = TRUE`,
		creator, marker+" Исполнителев"); err != nil {
		t.Fatalf("профиль исполнителя: %v", err)
	}
	general, err := projects.NewRepo(pool).CreateGeneral(ctx, projects.CreateGeneralInput{
		ClientID: client, SpecialistID: creator,
		Title:   marker + " общий",
		Brief:   "сделать к сроку",
		DueDate: time.Now().AddDate(0, 0, 7),
	})
	if err != nil {
		t.Fatalf("общий проект: %v", err)
	}
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, general.String())
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, general)
	})

	// Четыре выкладки, одна закрыта: прогресс — «1 из 4».
	for i := 0; i < 4; i++ {
		status := "planned"
		if i == 0 {
			status = "done"
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO project_publications (project_id, creator_user_id, due_date, status)
VALUES ($1, $2, CURRENT_DATE + $3::int, $4::publication_status)`,
			creators, creator, i, status); err != nil {
			t.Fatalf("выкладка %d: %v", i, err)
		}
	}
	s.defer_(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM project_publications WHERE project_id = $1`, creators)
	})

	_, body := s.get(t, "/api/v1/admin/projects?kind=creators_turnkey&q="+marker)
	items := list(t, body, "items")
	if len(items) != 1 {
		t.Fatalf("по виду creators_turnkey нашлось %d проектов, ожидали 1: %v", len(items), body)
	}
	item, _ := items[0].(map[string]any)
	if item["id"] != creators.String() {
		t.Fatalf("фильтр по виду вернул чужой проект: %v", item)
	}
	if got := num(t, item, "progress_done"); got != 1 {
		t.Errorf("сделано выкладок %d, ожидали 1", got)
	}
	if got := num(t, item, "progress_total"); got != 4 {
		t.Errorf("всего выкладок %d, ожидали 4", got)
	}
	if item["progress_unit"] != "publications" {
		t.Errorf("единица прогресса %v, ожидали publications", item["progress_unit"])
	}

	_, body = s.get(t, "/api/v1/admin/projects?kind=general&q="+marker)
	if got := num(t, body, "total"); got != 1 {
		t.Errorf("по виду general нашлось %d проектов, ожидали 1", got)
	}
	if items := list(t, body, "items"); len(items) == 1 {
		if m, _ := items[0].(map[string]any); m["id"] != general.String() {
			t.Errorf("фильтр general вернул чужой проект: %v", m)
		}
	}

	// Неизвестный вид — отказ с объяснением, а не молча весь список.
	code, body := s.get(t, "/api/v1/admin/projects?kind=unknown_kind")
	if code != http.StatusBadRequest {
		t.Fatalf("неизвестный вид: код %d, ожидали 400 (тело %v)", code, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "creators_turnkey") {
		t.Errorf("отказ по виду не перечисляет допустимые значения: %q", msg)
	}
	// И то же самое у сортировки — третий параметр той же ручки.
	code, body = s.get(t, "/api/v1/admin/projects?sort=by_wish")
	if code != http.StatusBadRequest {
		t.Fatalf("неизвестная сортировка: код %d, ожидали 400 (тело %v)", code, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "updated_asc") {
		t.Errorf("отказ по сортировке не перечисляет допустимые значения: %q", msg)
	}
}

// Без роли админа в оболочку не пускают. Проверяем на каждой новой
// ручке: забытая строка в роутере иначе открывает журнал всем.
func TestAdminShellRoutesRequireAdmin(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	manager, cleanup := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanup()

	for _, path := range []string{
		"/api/v1/admin/summary",
		"/api/v1/admin/team",
		"/api/v1/admin/audit",
		"/api/v1/admin/search?q=что-нибудь",
		"/api/v1/admin/users/" + manager.String(),
	} {
		code, body := h.Do(t, http.MethodGet, path, h.Token(t, manager), nil)
		if code != http.StatusForbidden {
			t.Errorf("%s под менеджером: код %d, ожидали 403 (тело %v)", path, code, body)
		}
	}
}
