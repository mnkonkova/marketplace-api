package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/orders"
	"marketpclce/tests/integration"
)

// setupOrderWorld — клиент, действующие правила с согласием и четверо
// креаторов. Возвращает id и уборку.
func setupOrderWorld(t *testing.T, pool *pgxpool.Pool) (clientID uuid.UUID, creators []uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	// Версия — заведомо старшая из существующих: CurrentTerms берёт
	// максимум по всей таблице, и случайный номер мог оказаться ниже
	// оставшегося от соседнего теста.
	var termsID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO terms_versions (version, body)
VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions), 'правила работы')
RETURNING id`).Scan(&termsID); err != nil {
		t.Fatalf("create terms: %v", err)
	}

	mkUser := func(kind string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, is_active, email_verified_at)
VALUES ($1, 'x', $2, TRUE, TRUE, now()) RETURNING id`,
			"orders-"+uuid.NewString()+"@example.com", kind).Scan(&id); err != nil {
			t.Fatalf("create user: %v", err)
		}
		return id
	}

	// Категория обязательна: креатор — это blogger или ugc. Без неё
	// человек не пройдёт отбор, и тесты падали бы на ровном месте.
	mkCreator := func() uuid.UUID {
		id := mkUser("specialist")
		if _, err := pool.Exec(ctx, `
INSERT INTO specialist_categories (user_id, category_code, is_primary)
VALUES ($1, 'blogger', TRUE)`, id); err != nil {
			t.Fatalf("set category: %v", err)
		}
		return id
	}

	clientID = mkUser("client")
	for i := 0; i < 4; i++ {
		creators = append(creators, mkCreator())
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO client_terms_consents (user_id, terms_version_id) VALUES ($1, $2)`,
		clientID, termsID); err != nil {
		t.Fatalf("consent: %v", err)
	}

	cleanup = func() {
		// Порядок важен: creator_orders ссылается на terms_versions без
		// ON DELETE, и удаление правил раньше заказов молча падало по
		// внешнему ключу. Оставшаяся строка становилась «действующей
		// версией» для следующих тестов, и они разваливались на
		// несогласованных правилах.
		_, _ = pool.Exec(ctx, `DELETE FROM creator_orders WHERE client_user_id = $1`, clientID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, clientID)
		for _, c := range creators {
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, c)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM client_terms_consents WHERE terms_version_id = $1`, termsID)
		if _, err := pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, termsID); err != nil {
			t.Errorf("не удалось убрать версию правил (останется мусор для соседних тестов): %v", err)
		}
	}
	return clientID, creators, cleanup
}

func nextMonth() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}

// Первый месяц — один креатор. Ограничение по КЛИЕНТУ, и клиент видит
// его до подбора, а не при попытке добавить второго.
func TestFirstMonthAllowsOneCreator(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	allowed, err := svc.AllowedCreators(ctx, clientID, now)
	if err != nil {
		t.Fatalf("AllowedCreators: %v", err)
	}
	if allowed != orders.FirstMonthCreators {
		t.Fatalf("доступно %d, ожидался %d", allowed, orders.FirstMonthCreators)
	}

	_, err = svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 2,
		VideosCount: 30, CreatorIDs: creators[:3],
	}, now)
	if !errors.Is(err, orders.ErrTooManyCreators) {
		t.Errorf("в первый месяц двое: got %v, want ErrTooManyCreators", err)
	}
}

// Приглашения уходят первым по приоритету и только на свободные места.
// Отказ немедленно двигает очередь дальше — клиент ничего не делает.
func TestInvitationAdvancesOnDecline(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:3],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.WithoutReserve {
		t.Error("в подборке трое при одном месте — резерв есть")
	}

	o, err := svc.SendInvitations(ctx, res.Order.ID, now)
	if err != nil {
		t.Fatalf("SendInvitations: %v", err)
	}
	invited := 0
	for _, c := range o.Candidates {
		if c.Status == orders.CandidateInvited {
			invited++
			if c.Priority != 1 {
				t.Errorf("приглашён приоритет %d, ожидался первый", c.Priority)
			}
		}
	}
	if invited != 1 {
		t.Fatalf("приглашено %d, ожидался 1: место одно", invited)
	}

	// Первый отказался — приглашение уходит второму само.
	o, err = svc.Respond(ctx, res.Order.ID, creators[0], false, now)
	if err != nil {
		t.Fatalf("Respond(отказ): %v", err)
	}
	for _, c := range o.Candidates {
		if c.CreatorUserID == creators[1] && c.Status != orders.CandidateInvited {
			t.Errorf("второй по приоритету не приглашён, статус %s", c.Status)
		}
	}

	// Второй согласился — состав укомплектован, третьего не зовут.
	o, err = svc.Respond(ctx, res.Order.ID, creators[1], true, now)
	if err != nil {
		t.Fatalf("Respond(согласие): %v", err)
	}
	if o.Status != orders.StatusStaffed {
		t.Errorf("статус %s, ожидался staffed", o.Status)
	}
	for _, c := range o.Candidates {
		if c.CreatorUserID == creators[2] && c.Status != orders.CandidateReserve {
			t.Errorf("третьего позвали на занятое место, статус %s", c.Status)
		}
	}
}

// Молчание трое суток — приглашение сгорает, место освобождается и
// уходит следующему. Через сутки об этом узнаёт менеджер, один раз.
func TestInvitationExpiresAndAdvances(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	repo := orders.NewRepo(pool)
	svc := orders.NewService(repo)
	now := time.Now().UTC()

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:2],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.SendInvitations(ctx, res.Order.ID, now); err != nil {
		t.Fatalf("SendInvitations: %v", err)
	}

	// Через сутки молчания — пинг менеджеру, приглашение ещё живо.
	_, pinged, err := svc.RunExpiry(ctx, now.Add(orders.ManagerPingAfter+time.Minute))
	if err != nil {
		t.Fatalf("RunExpiry(сутки): %v", err)
	}
	if pinged != 1 {
		t.Errorf("пингов менеджеру %d, ожидался 1", pinged)
	}
	// Второй проход — повторно не пишем.
	_, pinged, err = svc.RunExpiry(ctx, now.Add(orders.ManagerPingAfter+2*time.Minute))
	if err != nil {
		t.Fatalf("RunExpiry(повтор): %v", err)
	}
	if pinged != 0 {
		t.Errorf("менеджеру написали второй раз (%d)", pinged)
	}

	// Через трое суток приглашение сгорает и уходит следующему.
	expired, _, err := svc.RunExpiry(ctx, now.Add(orders.InviteTTL+time.Minute))
	if err != nil {
		t.Fatalf("RunExpiry(сгорание): %v", err)
	}
	if expired != 1 {
		t.Fatalf("сгорело %d приглашений, ожидалось 1", expired)
	}

	o, err := svc.Get(ctx, res.Order.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, c := range o.Candidates {
		switch c.CreatorUserID {
		case creators[0]:
			if c.Status != orders.CandidateExpired {
				t.Errorf("первый должен быть expired, а он %s", c.Status)
			}
		case creators[1]:
			if c.Status != orders.CandidateInvited {
				t.Errorf("второй должен быть приглашён, а он %s", c.Status)
			}
		}
	}
}

// Резерв кончился, состав не собран — это и есть момент «добрать».
// Менеджер добавляет людей, и приглашение уходит сразу.
func TestReserveExhaustedThenTopUp(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:1],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !res.WithoutReserve {
		t.Error("в подборке ровно одно место и один человек — резерва нет, клиента надо предупредить")
	}
	if _, err := svc.SendInvitations(ctx, res.Order.ID, now); err != nil {
		t.Fatalf("SendInvitations: %v", err)
	}

	o, err := svc.Respond(ctx, res.Order.ID, creators[0], false, now)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if o.NeedMore != 1 || o.ReserveLeft != 0 {
		t.Fatalf("нужно ещё %d, в резерве %d; ожидалось 1 и 0", o.NeedMore, o.ReserveLeft)
	}

	// Менеджер добирает — приглашение уходит немедленно.
	o, err = svc.AddCandidates(ctx, res.Order.ID, creators[1:2], now)
	if err != nil {
		t.Fatalf("AddCandidates: %v", err)
	}
	found := false
	for _, c := range o.Candidates {
		if c.CreatorUserID == creators[1] {
			found = true
			if c.Status != orders.CandidateInvited {
				t.Errorf("добранного не позвали, статус %s", c.Status)
			}
		}
	}
	if !found {
		t.Error("добранный не попал в подборку")
	}
}

// Занятого креатора нельзя поставить в подборку: иначе первым в списке
// окажется тот, кто взять не может, и заказ провисит трое суток впустую.
func TestBusyCreatorIsRejected(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	repo := orders.NewRepo(pool)
	svc := orders.NewService(repo)
	now := time.Now().UTC()
	month := nextMonth()

	if err := repo.SetAvailability(ctx, creators[0], month, false); err != nil {
		t.Fatalf("SetAvailability: %v", err)
	}

	_, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: month, Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:2],
	}, now)
	if !errors.Is(err, orders.ErrCreatorBusy) {
		t.Errorf("занятый креатор: got %v, want ErrCreatorBusy", err)
	}

	// Освободился — можно.
	if err := repo.SetAvailability(ctx, creators[0], month, true); err != nil {
		t.Fatalf("SetAvailability: %v", err)
	}
	if _, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: month, Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:2],
	}, now); err != nil {
		t.Errorf("свободный креатор должен приниматься: %v", err)
	}
}

// Без согласия с правилами заказ не создаётся.
func TestOrderRequiresConsent(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	// Опубликована новая версия правил — прежнее согласие её не покрывает.
	var newTerms uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO terms_versions (version, body)
VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions), 'новые правила')
RETURNING id`).Scan(&newTerms); err != nil {
		t.Fatalf("create terms: %v", err)
	}
	defer func() {
		// Порядок: на версию правил ссылаются и заказ, и СОГЛАСИЕ.
		// Про согласие легко забыть — оно создаётся не тестом, а
		// svc.Consent внутри проверяемого сценария.
		_, _ = pool.Exec(ctx, `DELETE FROM creator_orders WHERE client_user_id = $1`, clientID)
		_, _ = pool.Exec(ctx, `DELETE FROM client_terms_consents WHERE terms_version_id = $1`, newTerms)
		if _, err := pool.Exec(ctx, `DELETE FROM terms_versions WHERE id = $1`, newTerms); err != nil {
			t.Errorf("не удалось убрать версию правил: %v", err)
		}
	}()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	_, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:2],
	}, now)
	if !errors.Is(err, orders.ErrNoConsent) {
		t.Fatalf("новая версия правил: got %v, want ErrNoConsent", err)
	}

	if _, err := svc.Consent(ctx, clientID); err != nil {
		t.Fatalf("Consent: %v", err)
	}
	if _, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:2],
	}, now); err != nil {
		t.Errorf("после согласия заказ должен создаваться: %v", err)
	}
}

// В пакет блогеров идут только креаторы: blogger и ugc. Монтажёр — это
// другая модель работы и другие деньги, ему здесь не место.
func TestOnlyCreatorCategoriesAreAccepted(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	mk := func(category string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, is_active, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, TRUE, now()) RETURNING id`,
			"cat-"+uuid.NewString()+"@example.com").Scan(&id); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if category != "" {
			if _, err := pool.Exec(ctx, `
INSERT INTO specialist_categories (user_id, category_code, is_primary)
VALUES ($1, $2, TRUE)`, id, category); err != nil {
				t.Fatalf("set category: %v", err)
			}
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id) })
		return id
	}

	editor := mk("editor")
	uncategorized := mk("")
	ugc := mk("ugc")

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	for _, c := range []struct {
		name string
		id   uuid.UUID
	}{
		{"монтажёр", editor},
		{"без категории", uncategorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(ctx, orders.CreateOrderInput{
				ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
				VideosCount: 30, CreatorIDs: []uuid.UUID{c.id, creators[0]},
			}, now)
			if !errors.Is(err, orders.ErrNotACreator) {
				t.Errorf("got %v, want ErrNotACreator", err)
			}
		})
	}

	// UGC-креатор проходит наравне с блогером.
	if _, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: []uuid.UUID{ugc, creators[0]},
	}, now); err != nil {
		t.Errorf("ugc должен приниматься: %v", err)
	}
}

// Границы доступа в ручках заказа: чужой заказ не виден и не управляется,
// чужое приглашение не принимается.
func TestHarnessOrderAccessBoundaries(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()
	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:2],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.SendInvitations(ctx, res.Order.ID, now); err != nil {
		t.Fatalf("SendInvitations: %v", err)
	}

	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanupStranger()

	base := "/api/v1/me/orders/" + res.Order.ID.String()

	// Свой заказ виден.
	if code, body := h.Do(t, http.MethodGet, base, h.Token(t, clientID), nil); code != http.StatusOK {
		t.Fatalf("свой заказ: код %d, тело %v", code, body)
	}
	// Чужой — нет, и управлять им нельзя.
	for _, c := range []struct {
		name, method, path string
	}{
		{"чтение", http.MethodGet, base},
		{"приглашения", http.MethodPost, base + "/invite"},
		{"отмена", http.MethodPost, base + "/cancel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, _ := h.Do(t, c.method, c.path, h.Token(t, stranger), nil)
			if code != http.StatusNotFound {
				t.Errorf("чужой заказ: код %d, ожидался 404", code)
			}
		})
	}

	// Приглашён первый; второй ответить за него не может.
	code, body := h.Do(t, http.MethodPost,
		"/api/v1/me/creator/invitations/"+res.Order.ID.String()+"/respond",
		h.Token(t, creators[1]), map[string]any{"accept": true})
	if code != http.StatusConflict {
		t.Errorf("ответ за другого: код %d, ожидался 409. тело %v", code, body)
	}

	// Приглашённый отвечает — и состав собирается.
	code, body = h.Do(t, http.MethodPost,
		"/api/v1/me/creator/invitations/"+res.Order.ID.String()+"/respond",
		h.Token(t, creators[0]), map[string]any{"accept": true})
	if code != http.StatusOK {
		t.Fatalf("ответ приглашённого: код %d, тело %v", code, body)
	}
	if body["status"] != string(orders.StatusStaffed) {
		t.Errorf("статус %v, ожидался staffed", body["status"])
	}

	// Оплату отмечает менеджер, не клиент.
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	paidPath := "/api/v1/manager/orders/" + res.Order.ID.String() + "/paid"
	if code, _ := h.Do(t, http.MethodPost, paidPath, h.Token(t, clientID), nil); code != http.StatusForbidden {
		t.Errorf("клиент отмечает оплату: код %d, ожидался 403", code)
	}
	code, body = h.Do(t, http.MethodPost, paidPath, h.Token(t, manager), nil)
	if code != http.StatusOK {
		t.Fatalf("менеджер отмечает оплату: код %d, тело %v", code, body)
	}
	if body["status"] != string(orders.StatusPaid) {
		t.Errorf("статус %v, ожидался paid", body["status"])
	}
}

// Менеджерский экран состава: заказ достаётся по проекту, а очередь
// двигается вручную только на реально свободное место.
//
// Обе ручки появились под этот экран. Раньше заказ по проекту было
// нечем достать вовсе: /manager/orders отдаёт только застрявшие заказы
// (status = inviting и звать некого), а у проекта заказ давно оплачен и
// туда не попадает.
func TestManagerProjectOrderAndInvite(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := newAPIHarness(t, pool)

	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	repo := orders.NewRepo(pool)
	svc := orders.NewService(repo)
	now := time.Now().UTC()
	// Нужен один, в подборке трое: двое сидят в резерве, и именно их
	// очередь показывает менеджерский экран.
	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:3],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Проект заводит менеджер тем же запросом, что и интерфейс.
	code, body := h.Do(t, http.MethodPost, "/api/v1/manager/projects", h.Token(t, manager),
		map[string]any{
			"kind": "creators_turnkey", "title": "заказ → проект", "client_user_id": clientID,
		})
	if code != http.StatusCreated {
		t.Fatalf("создание проекта: код %d, тело %v", code, body)
	}
	projectID := body["id"].(string)
	// Убираем раньше пользователей: проект на них ссылается.
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID) }()

	orderPath := "/api/v1/manager/projects/" + projectID + "/order"

	// Проект ещё не привязан к заказу — заказа у него нет, и это 404,
	// а не сбой: заведённый руками проект из заказа не рос.
	if code, _ := h.Do(t, http.MethodGet, orderPath, h.Token(t, manager), nil); code != http.StatusNotFound {
		t.Errorf("проект без заказа: код %d, ожидался 404", code)
	}

	if err := repo.LinkProject(ctx, res.Order.ID, uuid.MustParse(projectID)); err != nil {
		t.Fatalf("LinkProject: %v", err)
	}

	code, body = h.Do(t, http.MethodGet, orderPath, h.Token(t, manager), nil)
	if code != http.StatusOK {
		t.Fatalf("заказ по проекту: код %d, тело %v", code, body)
	}
	if body["id"] != res.Order.ID.String() {
		t.Errorf("отдан заказ %v, ожидался %v", body["id"], res.Order.ID)
	}
	// Очередь приходит по приоритету: экран рисует её как есть, и
	// порядок здесь — это порядок приглашений, а не порядок выборки.
	cands, _ := body["candidates"].([]any)
	if len(cands) != 3 {
		t.Fatalf("кандидатов %d, ожидалось 3", len(cands))
	}
	for i, raw := range cands {
		c := raw.(map[string]any)
		if int(c["priority"].(float64)) != i+1 {
			t.Errorf("строка %d: приоритет %v, ожидался %d", i, c["priority"], i+1)
		}
	}

	// Приглашения ещё не уходили — заказ остался черновиком. Это тот
	// самый случай, когда очередь надо двинуть руками.
	invitePath := "/api/v1/manager/orders/" + res.Order.ID.String() + "/invite"
	code, body = h.Do(t, http.MethodPost, invitePath, h.Token(t, manager), nil)
	if code != http.StatusOK {
		t.Fatalf("приглашение следующего: код %d, тело %v", code, body)
	}
	if body["status"] != string(orders.StatusInviting) {
		t.Errorf("статус %v, ожидался inviting", body["status"])
	}
	cands, _ = body["candidates"].([]any)
	want := []string{"invited", "reserve", "reserve"}
	for i, raw := range cands {
		if got := raw.(map[string]any)["status"]; got != want[i] {
			t.Errorf("строка %d: статус %v, ожидался %s", i, got, want[i])
		}
	}

	// Второй раз звать некого: место одно и оно уже ждёт ответа.
	// Молчаливый успех здесь означал бы приглашение сверх мест.
	code, body = h.Do(t, http.MethodPost, invitePath, h.Token(t, manager), nil)
	if code != http.StatusConflict {
		t.Errorf("повторное приглашение: код %d, ожидался 409. тело %v", code, body)
	}
	if body["error"] != "no_free_slot" {
		t.Errorf("код ошибки %v, ожидался no_free_slot", body["error"])
	}

	// Заказчику эти ручки недоступны — они менеджерские.
	if code, _ := h.Do(t, http.MethodGet, orderPath, h.Token(t, clientID), nil); code != http.StatusForbidden {
		t.Errorf("заказчик читает заказ по проекту: код %d, ожидался 403", code)
	}
}
