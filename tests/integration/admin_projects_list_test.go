package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Админский список проектов: поиск, фильтры, сортировка, страница.
//
// Проверяем именно через HTTP, а не через repo: половина поведения живёт
// в разборе query-параметров (manager=none, include_test, дефолты limit и
// sort), и тест на repo прошёл бы мимо неё.
//
// Все проекты помечаем уникальным маркером в названии и просим список с
// q=<маркер>: тестовая база общая, и без такого сужения любой соседний
// тест, оставивший проект, менял бы здесь ожидания.

// adminListSeed — набор проектов одного прогона плюс уборка за ним.
type adminListSeed struct {
	marker   string
	admin    string // токен админа
	manager  uuid.UUID
	client   uuid.UUID
	byTitle  map[string]uuid.UUID
	harness  *integration.APIHarness
	cleanups []func()
}

func (s *adminListSeed) cleanup() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

// project — заводит проект напрямую через repo. Вид creators_turnkey взят
// не случайно: воронки у него нет, и не приходится поднимать pipeline ради
// проверки списка.
func (s *adminListSeed) project(t *testing.T, in projects.StartProjectInput) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	in.Kind = projects.KindCreatorsTurnkey
	in.Source = projects.SourceManual
	id, err := projects.NewRepo(s.harness.Pool).StartProject(ctx, in)
	if err != nil {
		t.Fatalf("создать проект %q: %v", in.Title, err)
	}
	s.byTitle[in.Title] = id
	s.cleanups = append(s.cleanups, func() {
		_, _ = s.harness.Pool.Exec(ctx,
			`DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, id.String())
		_, _ = s.harness.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// touch — переписывает updated_at, чтобы проверять порядок «что не
// двигалось». Через SQL: настоящего способа состарить проект нет.
func (s *adminListSeed) touch(t *testing.T, id uuid.UUID, ago time.Duration) {
	t.Helper()
	if _, err := s.harness.Pool.Exec(context.Background(),
		`UPDATE projects SET updated_at = now() - $2::interval WHERE id = $1`,
		id, fmt.Sprintf("%d seconds", int(ago.Seconds()))); err != nil {
		t.Fatalf("состарить проект: %v", err)
	}
}

func (s *adminListSeed) list(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	return s.harness.Do(t, http.MethodGet, "/api/v1/admin/projects?"+query, s.admin, nil)
}

// titles — названия проектов страницы в том порядке, в каком их вернул
// сервер.
func titles(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("в ответе нет items: %v", body)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		s, _ := m["title"].(string)
		out = append(out, s)
	}
	return out
}

func total(t *testing.T, body map[string]any) int {
	t.Helper()
	v, ok := body["total"].(float64)
	if !ok {
		t.Fatalf("в ответе нет total: %v", body)
	}
	return int(v)
}

func newAdminListSeed(t *testing.T, pool *pgxpool.Pool) *adminListSeed {
	t.Helper()
	h := newAPIHarness(t, pool)
	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	client, cleanupClient := h.NewUser(t, userOpts{Kind: "client"})

	s := &adminListSeed{
		// Маркер уникален на прогон: тестовая база общая, соседние тесты
		// оставляют в ней свои проекты.
		marker:   "mrk" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""),
		admin:    h.Token(t, admin),
		manager:  manager,
		client:   client,
		byTitle:  map[string]uuid.UUID{},
		harness:  h,
		cleanups: []func(){cleanupClient, cleanupManager, cleanupAdmin},
	}
	return s
}

// Поиск ищет и по названию проекта, и по клиенту — обоих видов:
// зарегистрированному (имя в client_profiles) и без аккаунта (имя лежит
// прямо на проекте).
func TestAdminProjectsSearch(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminListSeed(t, pool)
	defer s.cleanup()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
INSERT INTO client_profiles (user_id, display_name) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET display_name = EXCLUDED.display_name`,
		s.client, "Ромашка Групп"); err != nil {
		t.Fatalf("профиль клиента: %v", err)
	}

	byTitle := s.marker + " ролик про корм"
	byRegistered := s.marker + " съёмка"
	byNoAccount := s.marker + " монтаж"
	s.project(t, projects.StartProjectInput{Title: byTitle,
		ClientName: "Кто-то ещё", ClientContact: "+70000000000"})
	s.project(t, projects.StartProjectInput{Title: byRegistered, ClientUserID: &s.client})
	s.project(t, projects.StartProjectInput{Title: byNoAccount,
		ClientName: "Подсолнух ООО", ClientContact: "@podsolnuh"})

	cases := []struct {
		name  string
		q     string
		want  []string
		exact bool
	}{
		{"по слову из названия", s.marker + "+ролик", []string{byTitle}, true},
		{"по имени клиента с аккаунтом", "Ромашка", []string{byRegistered}, false},
		{"по имени клиента без аккаунта", "Подсолнух", []string{byNoAccount}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := s.list(t, "q="+c.q)
			if code != http.StatusOK {
				t.Fatalf("код %d, тело %v", code, body)
			}
			got := titles(t, body)
			for _, want := range c.want {
				if !contains(got, want) {
					t.Fatalf("проект %q не нашёлся по q=%q; нашлись: %v", want, c.q, got)
				}
			}
			if c.exact && len(got) != len(c.want) {
				t.Fatalf("по q=%q ожидались только %v, пришли %v", c.q, c.want, got)
			}
		})
	}

	// Односимвольный запрос совпадает со всем подряд — от него толку нет,
	// и мы его игнорируем, а не отдаём пустоту.
	code, body := s.list(t, "q=%D1%8F") // «я»
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	if total(t, body) < 3 {
		t.Fatalf("короткий q должен игнорироваться, а не фильтровать: total=%d", total(t, body))
	}
}

// Фильтры по ответственному и по статусу считаются на сервере: страница
// с limit=1 обязана отдать правильный total под фильтром, а не число
// строк, доехавших до браузера.
func TestAdminProjectsFilters(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminListSeed(t, pool)
	defer s.cleanup()

	mine := s.marker + " мой"
	foreign := s.marker + " ничей"
	paused := s.marker + " на паузе"
	s.project(t, projects.StartProjectInput{Title: mine, ClientUserID: &s.client,
		AssignedToUserID: &s.manager})
	s.project(t, projects.StartProjectInput{Title: foreign, ClientUserID: &s.client})
	pausedID := s.project(t, projects.StartProjectInput{Title: paused, ClientUserID: &s.client,
		AssignedToUserID: &s.manager})
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET status = 'on_hold' WHERE id = $1`, pausedID); err != nil {
		t.Fatalf("поставить on_hold: %v", err)
	}

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"по менеджеру", "manager=" + s.manager.String(), []string{mine, paused}},
		{"без ответственного", "manager=none", []string{foreign}},
		{"по статусу", "status=on_hold", []string{paused}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := s.list(t, "q="+s.marker+"&"+c.query)
			if code != http.StatusOK {
				t.Fatalf("код %d, тело %v", code, body)
			}
			got := titles(t, body)
			if len(got) != len(c.want) || total(t, body) != len(c.want) {
				t.Fatalf("под фильтром %q ожидались %v, пришли %v (total=%d)",
					c.query, c.want, got, total(t, body))
			}
			for _, want := range c.want {
				if !contains(got, want) {
					t.Fatalf("под фильтром %q не нашёлся %q; пришли %v", c.query, want, got)
				}
			}
		})
	}

	// Колонка «Менеджер» в списке: без имени ответственного по нему не
	// ответить на вторую половину вопроса «что встало и кто за это отвечает».
	code, body := s.list(t, "q="+s.marker+"&manager="+s.manager.String())
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		if name, _ := m["manager_display_name"].(string); name == "" {
			t.Fatalf("у проекта %v пустое manager_display_name", m["title"])
		}
	}

	// Кривой manager — 400 с объяснением, а не молчаливый «весь список».
	code, body = s.list(t, "manager=не-uuid")
	if code != http.StatusBadRequest {
		t.Fatalf("manager=мусор: код %d, ожидался 400. тело %v", code, body)
	}
}

// Тестовые проекты по умолчанию скрыты. Именно из-за них половина
// админского списка была «тест т8т 1234».
func TestAdminProjectsHideTestByDefault(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminListSeed(t, pool)
	defer s.cleanup()

	real := s.marker + " настоящий"
	fake := s.marker + " тест т8т"
	s.project(t, projects.StartProjectInput{Title: real, ClientUserID: &s.client})
	s.project(t, projects.StartProjectInput{Title: fake, ClientUserID: &s.client, IsTest: true})

	code, body := s.list(t, "q="+s.marker)
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	if got := titles(t, body); len(got) != 1 || got[0] != real {
		t.Fatalf("по умолчанию должен быть только %q, пришли %v", real, got)
	}
	if total(t, body) != 1 {
		t.Fatalf("total должен считать тестовые наравне со списком: %d", total(t, body))
	}

	code, body = s.list(t, "q="+s.marker+"&include_test=true")
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	got := titles(t, body)
	if len(got) != 2 || !contains(got, fake) {
		t.Fatalf("с include_test=true ожидались оба проекта, пришли %v", got)
	}
}

// Сортировка и страница. Главный случай — «самые давно обновлённые
// сверху»: ради него список и открывают.
func TestAdminProjectsSortAndPaging(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminListSeed(t, pool)
	defer s.cleanup()

	oldest := s.marker + " забытый"
	middle := s.marker + " вчерашний"
	newest := s.marker + " свежий"
	s.touch(t, s.project(t, projects.StartProjectInput{Title: oldest, ClientUserID: &s.client}),
		30*24*time.Hour)
	s.touch(t, s.project(t, projects.StartProjectInput{Title: middle, ClientUserID: &s.client}),
		24*time.Hour)
	s.touch(t, s.project(t, projects.StartProjectInput{Title: newest, ClientUserID: &s.client}),
		time.Minute)

	code, body := s.list(t, "q="+s.marker+"&sort=updated_asc")
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	if got := titles(t, body); !sameOrder(got, []string{oldest, middle, newest}) {
		t.Fatalf("sort=updated_asc: ожидался порядок от давних к свежим, пришло %v", got)
	}

	code, body = s.list(t, "q="+s.marker+"&sort=updated_desc")
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	if got := titles(t, body); !sameOrder(got, []string{newest, middle, oldest}) {
		t.Fatalf("sort=updated_desc: ожидался обратный порядок, пришло %v", got)
	}

	// Страница режется на сервере: total считает все совпадения, items —
	// только запрошенную страницу. Если резать на фронте, total совпал бы
	// с длиной items.
	code, body = s.list(t, "q="+s.marker+"&sort=updated_asc&limit=2&offset=0")
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	if got := titles(t, body); !sameOrder(got, []string{oldest, middle}) {
		t.Fatalf("первая страница: ожидались %v, пришло %v", []string{oldest, middle}, got)
	}
	if total(t, body) != 3 {
		t.Fatalf("total должен считать все совпадения, а не строки страницы: %d", total(t, body))
	}

	code, body = s.list(t, "q="+s.marker+"&sort=updated_asc&limit=2&offset=2")
	if code != http.StatusOK {
		t.Fatalf("код %d, тело %v", code, body)
	}
	if got := titles(t, body); !sameOrder(got, []string{newest}) {
		t.Fatalf("вторая страница: ожидался %q, пришло %v", newest, got)
	}

	// Неизвестный порядок — 400: молча отдать список в другом порядке
	// хуже, чем сказать, что параметр не понят.
	code, body = s.list(t, "sort=по_настроению")
	if code != http.StatusBadRequest {
		t.Fatalf("sort=мусор: код %d, ожидался 400. тело %v", code, body)
	}
}

// Название проекта: минимум три символа, считается после trim. Без этого
// в списке заводятся «12345675432» и «  ы  ».
func TestAdminCreateProjectTitleValidation(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminListSeed(t, pool)
	defer s.cleanup()

	create := func(title string) (int, map[string]any) {
		return s.harness.Do(t, http.MethodPost, "/api/v1/admin/projects", s.admin, map[string]any{
			"kind":           "creators_turnkey",
			"client_user_id": s.client.String(),
			"title":          title,
		})
	}

	for _, bad := range []string{"", "  ", "ы", "ab", "  ab  ", "\t\n"} {
		code, body := create(bad)
		if code != http.StatusBadRequest {
			// Проект мог создаться — уберём, чтобы не осталось мусора.
			if id, ok := body["id"].(string); ok {
				if pid, err := uuid.Parse(id); err == nil {
					_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
				}
			}
			t.Fatalf("название %q принято с кодом %d, ожидался 400", bad, code)
		}
		if msg, _ := body["message"].(string); msg == "" {
			t.Fatalf("отказ на %q без объяснения: %v", bad, body)
		}
	}

	// Ровно три символа — уже название. И пробелы по краям срезаются, а не
	// идут в базу.
	code, body := create("  " + s.marker + "  ")
	if code != http.StatusCreated {
		t.Fatalf("нормальное название отклонено: код %d, тело %v", code, body)
	}
	if id, ok := body["id"].(string); ok {
		pid, _ := uuid.Parse(id)
		s.cleanups = append(s.cleanups, func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, id)
			_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
		})
	}
	if got, _ := body["title"].(string); got != s.marker {
		t.Fatalf("название сохранено как %q, ожидалось обрезанное %q", got, s.marker)
	}
}

func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
