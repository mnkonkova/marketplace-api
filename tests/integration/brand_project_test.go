package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/billing"
	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Проект «бренд под ключ»: план выкладок есть, людей нет.
//
// Главный риск всей этой ветки тихий: pgx скан NULL в не-указательный
// uuid.UUID падает В РАНТАЙМЕ, компилятор про это не знает. Поэтому
// половина тестов ниже не проверяет числа, а просто проходит по каждой
// выборке, которая читает creator_user_id: список, отчёт, лента
// заказчика, календарь, напоминания, «ролик не пошёл». Зелёный прогон
// здесь означает «ни одна из них не падает на NULL».

// setupBrandProject — проект вида brand_turnkey без единого креатора.
func setupBrandProject(t *testing.T, pool *pgxpool.Pool) (projectID uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	_, _, projectID, baseCleanup := setupPipelineAndProject(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET kind = 'brand_turnkey' WHERE id = $1`, projectID); err != nil {
		t.Fatalf("set project kind: %v", err)
	}
	return projectID, func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, projectID)
		baseCleanup()
	}
}

// brandManager — менеджер, назначенный на проект. Без назначения все
// менеджерские ручки отвечают 404, и тест проверял бы не то.
func brandManager(t *testing.T, h *integration.APIHarness, projectID uuid.UUID) (uuid.UUID, func()) {
	t.Helper()
	manager, cleanup := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE projects SET assigned_to_user_id = $1 WHERE id = $2`, manager, projectID); err != nil {
		t.Fatalf("назначение менеджера: %v", err)
	}
	return manager, cleanup
}

// Пачка без креаторов — норма: даты есть, поручать их некому.
func TestBrandBatchCreatesPublicationsWithoutCreator(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)
	author, cleanupAuthor := integration.NewAPIHarness(t, pool).NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupAuthor()

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{pubDay(1), pubDay(3), pubDay(5)},
		CreatedBy: author,
	})
	if err != nil {
		t.Fatalf("CreateBatch без креаторов: %v", err)
	}
	if res.Created != 3 {
		t.Fatalf("создано %d выкладок, ожидалось 3 — произведение «креаторы × даты» должно выродиться в даты",
			res.Created)
	}
	for _, p := range res.Items {
		if p.CreatorUserID != nil {
			t.Errorf("у выкладки %s появился владелец %s — ролик принадлежит проекту, а не человеку",
				p.ID, *p.CreatorUserID)
		}
	}

	// Список — первая выборка, которая сканирует creator_user_id. До
	// перевода поля в указатель она падала здесь же.
	list, err := repo.ListByProject(ctx, projectID)
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("в списке %d выкладок, ожидалось 3", len(list))
	}
	for _, p := range list {
		if p.CreatorUserID != nil || p.CreatorName != "" {
			t.Errorf("выкладке %s приписали креатора %v (%q)", p.ID, p.CreatorUserID, p.CreatorName)
		}
	}
}

// Креаторы в пачку этого вида — отказ, а не тихое «поняли по-своему».
func TestBrandBatchRejectsCreators(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	someone := uuid.New()
	_, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{someone},
		Dates:          []time.Time{pubDay(1)},
		CreatedBy:      someone,
	})
	if !errors.Is(err, publications.ErrCrewNotAllowed) {
		t.Errorf("менеджер прислал креаторов в проект без креаторов: ждали ErrCrewNotAllowed, got %v", err)
	}
}

// Один ролик = один день: (project_id, due_date) уникально там, где
// креатора нет.
//
// В уникальном индексе Postgres считает NULL'ы различными, поэтому
// старый publications_creator_day_uniq здесь не защищал ничего: два
// прохода в одну секунду завели бы два ролика на один день.
func TestBrandOneVideoPerDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	h := integration.NewAPIHarness(t, pool)
	author, cleanupAuthor := brandManager(t, h, projectID)
	defer cleanupAuthor()

	svc := publications.NewService(publications.NewRepo(pool))
	day := pubDay(2)

	first, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{day},
		CreatedBy: author,
	})
	if err != nil || first.Created != 1 {
		t.Fatalf("первая выкладка: created=%d err=%v", first.Created, err)
	}

	// Повтор пачки на тот же день гасится ON CONFLICT DO NOTHING —
	// ровно так же, как у проекта с креаторами при двойном клике.
	second, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{day},
		CreatedBy: author,
	})
	if err != nil {
		t.Fatalf("повторная пачка: %v", err)
	}
	if second.Created != 0 {
		t.Errorf("на тот же день завелось ещё %d выкладок — индекс по (project_id, due_date) не работает",
			second.Created)
	}

	// И тем же ограничением — поштучное добавление.
	_, err = svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		Day:           day,
		ManagerUserID: author,
		Now:           time.Now().UTC(),
	})
	if !errors.Is(err, publications.ErrDayTaken) {
		t.Errorf("второй ролик на тот же день: ждали ErrDayTaken, got %v", err)
	}
}

// Ссылки с разных площадок сводятся в один ролик — по дню.
//
// Это и есть «агрегация по дате»: у проекта без креаторов ключом склейки
// служит день, а не пара «креатор + ближайшая открытая выкладка».
func TestBrandPlatformsMergeIntoOneVideoByDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	h := integration.NewAPIHarness(t, pool)
	manager, cleanupManager := brandManager(t, h, projectID)
	defer cleanupManager()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{pubDay(0)},
		CreatedBy: manager,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	// Ссылки вставляет менеджер: креаторской сдачи у этого вида нет.
	for _, u := range []string{
		"https://www.tiktok.com/@brand/video/777",
		"https://www.youtube.com/shorts/aaaaaaaaaaa",
	} {
		if _, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Platform:      platformOf(t, u),
			URL:           u,
		}); err != nil {
			t.Fatalf("ManagerEditLink %s: %v", u, err)
		}
	}

	got, err := repo.Get(ctx, pubID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Links) != 2 {
		t.Fatalf("у ролика %d площадок, ожидалось 2 — площадки должны склеиться в одну выкладку дня",
			len(got.Links))
	}
	if got.Status != publications.StatusPartial {
		t.Errorf("статус %s, ожидался partial: пришли две площадки из пяти", got.Status)
	}
	if got.CreatorUserID != nil {
		t.Error("сдача ссылок приписала ролику владельца")
	}

	// Креаторская сдача по этому ролику обязана отказывать, а не падать.
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   manager,
		URLs:          []string{"https://vk.com/video-1_2"},
	})
	if !errors.Is(err, publications.ErrForbidden) {
		t.Errorf("креаторская сдача по ролику без креатора: ждали ErrForbidden, got %v", err)
	}
}

// platformOf — площадка по адресу. Разбор уже написан, повторять его в
// тесте значило бы проверять свою копию правила, а не общую.
func platformOf(t *testing.T, rawURL string) string {
	t.Helper()
	l, err := publications.ParseLink(rawURL)
	if err != nil {
		t.Fatalf("ParseLink %s: %v", rawURL, err)
	}
	return l.Platform
}

// Каждая выборка, читающая creator_user_id, переживает NULL.
//
// Проверяем проходом, а не числами: смысл теста в том, что ни одна из
// них не отвечает ошибкой скана.
func TestBrandReadsSurviveNullCreator(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	h := integration.NewAPIHarness(t, pool)
	manager, cleanupManager := brandManager(t, h, projectID)
	defer cleanupManager()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{pubDay(-1)},
		CreatedBy: manager,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID
	if _, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
		PublicationID: pubID,
		ManagerUserID: manager,
		Platform:      "tiktok",
		URL:           "https://www.tiktok.com/@brand/video/888",
	}); err != nil {
		t.Fatalf("ManagerEditLink: %v", err)
	}

	rep, err := svc.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.ByCreator) != 0 {
		t.Errorf("в отчёте %d строк «по креаторам» — делить не по кому", len(rep.ByCreator))
	}
	if len(rep.VideoRows) != 1 {
		t.Fatalf("строк роликов %d, ожидалась 1", len(rep.VideoRows))
	}
	if rep.VideoRows[0].CreatorUserID != nil {
		t.Error("строке ролика приписан креатор")
	}

	feed, err := svc.ClientFeed(ctx, projectID, 20)
	if err != nil {
		t.Fatalf("ClientVideos: %v", err)
	}
	if len(feed) != 1 || feed[0].CreatorUserID != nil {
		t.Errorf("лента заказчика: %d роликов, creator=%v", len(feed), feed[0].CreatorUserID)
	}

	if _, err := svc.Calendar(ctx, projectID, time.Now().UTC()); err != nil {
		t.Fatalf("Calendar: %v", err)
	}

	// Напоминания: выкладка вчерашняя и незакрытая, значит просроченная.
	// Она обязана попасть в выборку (иначе метрика и сводка врут) и не
	// уронить её сканом NULL.
	rems, err := repo.DueReminders(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("DueReminders: %v", err)
	}
	var mine *publications.Reminder
	for i := range rems {
		if rems[i].PublicationID == pubID {
			mine = &rems[i]
		}
	}
	if mine == nil {
		t.Fatal("просроченная выкладка проекта без креаторов не попала в напоминания")
	}
	if mine.CreatorUserID != nil {
		t.Error("у напоминания появился адресат-человек, которого в проекте нет")
	}

	// Ручное «напомнить» по такому ролику — отказ, а не письмо в никуда.
	if _, err := svc.RemindNow(ctx, pubID, time.Now().UTC()); !errors.Is(err, publications.ErrNoCreator) {
		t.Errorf("RemindNow по ролику без креатора: ждали ErrNoCreator, got %v", err)
	}

	// Проход планировщика целиком: он и складывает сводку менеджерам.
	if _, err := svc.RunReminders(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("RunReminders: %v", err)
	}
	if _, err := repo.DueWeakVideos(ctx, time.Now().UTC(), 10); err != nil {
		t.Fatalf("DueWeakVideos: %v", err)
	}
}

// Выключенные блоки отвечают 409 с объяснением, а не пустотой.
//
// Пустой состав читается как «ещё никого не добавили», пустой чек-лист —
// как «забыли подключить». Здесь их не будет никогда, и разница между
// «пока нет» и «не бывает» — это разница между «подожду» и «не жди».
func TestBrandDisabledBlocksRefuseExplicitly(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	h := integration.NewAPIHarness(t, pool)
	manager, cleanupManager := brandManager(t, h, projectID)
	defer cleanupManager()
	token := h.Token(t, manager)

	base := "/api/v1/manager/projects/" + projectID.String()
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"состав проекта", http.MethodGet, base + "/creators", nil},
		{"добавить креатора", http.MethodPost, base + "/creators",
			map[string]any{"creator_user_id": uuid.New().String()}},
		{"чек-лист проекта", http.MethodGet, base + "/checklist", nil},
		{"пункт чек-листа", http.MethodPost, base + "/checklist/items",
			map[string]any{"text": "снять горизонтально"}},
		{"пересчёт начислений", http.MethodPost, base + "/accruals/recalc", nil},
		{"периоды", http.MethodGet, base + "/billing/periods", nil},
		{"подтвердить конец периода", http.MethodPost, base + "/billing/confirm_period_end", nil},
		{"подписчики", http.MethodGet, base + "/subscribers", nil},
	}
	for _, c := range cases {
		code, body := h.Do(t, c.method, c.path, token, c.body)
		if code != http.StatusConflict {
			t.Errorf("%s: код %d, ожидался 409 — блока у этого вида нет", c.name, code)
			continue
		}
		if body["error"] != "wrong_project_kind" {
			t.Errorf("%s: код ошибки %v, ожидался wrong_project_kind", c.name, body["error"])
		}
		if msg, _ := body["message"].(string); msg == "" {
			t.Errorf("%s: отказ без объяснения — на экране это читается как поломка прав", c.name)
		}
	}

	// Проверка ролика — по id выкладки, а не проекта.
	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{pubDay(0)},
		CreatedBy: manager,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/publications/"+res.Items[0].ID.String()+"/review", token,
		map[string]any{"decision": "accepted"})
	if code != http.StatusConflict || body["error"] != "wrong_project_kind" {
		t.Errorf("проверка ролика: код %d, ошибка %v — проверять работу не у кого",
			code, body["error"])
	}
}

// Одна дата ставится ручкой, а не только пачкой.
//
// Живой месяц правят по одной клетке: перенесли съёмку, добавили ролик.
// Ручка отказывала на пустом creator_user_id ещё до чтения вида
// проекта — «Нужны creator_user_id и due_date», 400, — хотя репозиторий
// за ней умел вставлять выкладку без владельца с самого начала. Со
// стороны это выглядело так, будто проект без креаторов заводится, но
// не ведётся: пачка есть, правки нет.
//
// Проверяем обе стороны развилки: у проекта без креаторов пустое поле —
// норма, у проекта с креаторами оно означает «никого не назвали», и
// отказ должен объяснять именно это, а не «тело не разобрали».
func TestBrandAddPublicationWithoutCreator(t *testing.T) {
	pool := integration.Pool(t)
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	h := integration.NewAPIHarness(t, pool)
	manager, cleanupManager := brandManager(t, h, projectID)
	defer cleanupManager()
	token := h.Token(t, manager)

	path := "/api/v1/manager/projects/" + projectID.String() + "/publications"
	day := pubDay(3).Format("2006-01-02")

	// Пустая строка, а не отсутствие ключа: ровно это шлёт форма, и
	// разобрать её в uuid.UUID нельзя — на этом запрос и падал.
	code, body := h.Do(t, http.MethodPost, path, token,
		map[string]any{"creator_user_id": "", "due_date": day})
	if code != http.StatusCreated {
		t.Fatalf("выкладка без владельца: код %d, тело %v — ожидали 201", code, body)
	}
	if owner, ok := body["creator_user_id"]; ok && owner != nil {
		t.Errorf("у выкладки такого проекта появился владелец: %v", owner)
	}

	// Ключа нет вовсе — то же самое.
	code, body = h.Do(t, http.MethodPost, path, token,
		map[string]any{"due_date": pubDay(4).Format("2006-01-02")})
	if code != http.StatusCreated {
		t.Errorf("выкладка без ключа creator_user_id: код %d, тело %v", code, body)
	}

	// А мусор в поле остаётся ошибкой формата — и называется ею.
	code, body = h.Do(t, http.MethodPost, path, token,
		map[string]any{"creator_user_id": "не-uuid", "due_date": day})
	if code != http.StatusBadRequest || body["error"] != "bad_id" {
		t.Errorf("мусор в creator_user_id: код %d, ошибка %v — ожидали 400 bad_id", code, body["error"])
	}
}

// У проекта С креаторами пустое поле — это «никого не назвали», и
// отказ обязан говорить про состав, а не про формат тела.
func TestCreatorsProjectAddPublicationNeedsCreator(t *testing.T) {
	pool := integration.Pool(t)
	_, _, projectID, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	// Посев заводит проект по воронке, а выкладки бывают только у двух
	// видов: без этого ручка отвечала бы «у этого вида выкладок нет», и
	// тест проверял бы не ту развилку.
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET kind = 'creators_turnkey' WHERE id = $1`, projectID); err != nil {
		t.Fatalf("set project kind: %v", err)
	}

	h := integration.NewAPIHarness(t, pool)
	manager, cleanupManager := brandManager(t, h, projectID)
	defer cleanupManager()
	token := h.Token(t, manager)

	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+projectID.String()+"/publications", token,
		map[string]any{"creator_user_id": "", "due_date": pubDay(3).Format("2006-01-02")})
	if code != http.StatusConflict || body["error"] != "creator_not_in_project" {
		t.Errorf("выкладка без креатора в проекте с креаторами: код %d, ошибка %v — ожидали 409 creator_not_in_project",
			code, body["error"])
	}
}

// Матрица возможностей и ручки говорят одно и то же.
//
// Тест на согласованность, а не на значения: если FeaturesOf скажет
// «чек-лист есть», а ручка продолжит отвечать 409, разъедутся экран и
// бэк, и виноватым будет выглядеть фронт.
func TestBrandFeaturesMatchHandlers(t *testing.T) {
	f := projects.FeaturesOf(projects.KindBrandTurnkey)
	if !f.HasPublications || !f.HasAccounts || !f.HasMaterials || !f.HasManualCost {
		t.Errorf("бренд под ключ потерял свои блоки: %+v", f)
	}
	if f.HasCrew || f.HasReview || f.HasChecklist || f.HasBilling || f.HasFunnel {
		t.Errorf("бренду под ключ включили блок, которого у него нет: %+v", f)
	}
	if f.PingTarget != projects.PingManagersChat {
		t.Errorf("адресат автопингов %q, ожидался общий чат менеджеров", f.PingTarget)
	}
}

// Менеджер вводит стоимость — и по ней считается СПВ.
//
// У проекта с креаторами делимое складывается из начислений людям; здесь
// складывать нечего, и сумму называет менеджер. Хранится она в том же
// снимке условий project_billing — второй денежной модели у проекта не
// заводится.
func TestBrandManagerCostGivesCostPerView(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, cleanup := setupBrandProject(t, pool)
	defer cleanup()

	h := integration.NewAPIHarness(t, pool)
	manager, cleanupManager := brandManager(t, h, projectID)
	defer cleanupManager()
	token := h.Token(t, manager)

	svc := publications.NewService(publications.NewRepo(pool))
	repo := publications.NewRepo(pool)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{pubDay(0)},
		CreatedBy: manager,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pub, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
		PublicationID: res.Items[0].ID,
		ManagerUserID: manager,
		Platform:      "tiktok",
		URL:           "https://www.tiktok.com/@brand/video/999",
	})
	if err != nil {
		t.Fatalf("ManagerEditLink: %v", err)
	}
	views := int64(200_000)
	likes, comments := int64(1000), int64(100)
	link := publications.LinkToCollect{
		LinkID:        pub.Links[0].ID,
		PublicationID: pub.ID,
		ProjectID:     projectID,
		Platform:      pub.Links[0].Platform,
		URL:           pub.Links[0].URLCanonical,
		SubmittedAt:   pub.Links[0].SubmittedAt,
	}
	if err := repo.SaveStats(ctx, link, &views, &likes, &comments, nil, nil,
		time.Now().UTC()); err != nil {
		t.Fatalf("SaveStats: %v", err)
	}

	// Пока сумму не назвали, СПВ не показывается вовсе: ноль читался бы
	// как «бесплатно».
	rep, err := svc.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report до ввода стоимости: %v", err)
	}
	if rep.Cost != nil || rep.CostPer1000 != nil {
		t.Errorf("стоимость не вводили, а в отчёте cost=%v cpv=%v", rep.Cost, rep.CostPer1000)
	}

	// 50 000 ₽ за период, в копейках.
	const cost = int64(5_000_000)
	code, _ := h.Do(t, http.MethodPut, "/api/v1/manager/projects/"+projectID.String()+"/billing",
		token, map[string]any{"project_cost": cost})
	if code != http.StatusOK {
		t.Fatalf("сохранение стоимости: код %d", code)
	}

	terms, err := billing.NewRepo(pool).Terms(ctx, projectID)
	if err != nil {
		t.Fatalf("Terms: %v", err)
	}
	if terms.ProjectCost != cost {
		t.Errorf("в снимке условий стоимость %d, ожидалось %d", terms.ProjectCost, cost)
	}

	rep, err = svc.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report после ввода стоимости: %v", err)
	}
	if rep.Cost == nil || *rep.Cost != cost {
		t.Fatalf("в отчёте стоимость %v, ожидалось %d", rep.Cost, cost)
	}
	// 5 000 000 копеек на 200 000 просмотров — 25 000 копеек за тысячу.
	if rep.CostPer1000 == nil || *rep.CostPer1000 != 25_000 {
		t.Errorf("СПВ %v, ожидалось 25000 копеек за тысячу просмотров", rep.CostPer1000)
	}

	// Отрицательную сумму не принимаем: молча записав её, показали бы
	// заказчику отрицательный СПВ.
	code, body := h.Do(t, http.MethodPut, "/api/v1/manager/projects/"+projectID.String()+"/billing",
		token, map[string]any{"project_cost": -1})
	if code != http.StatusBadRequest {
		t.Errorf("отрицательная стоимость: код %d, ожидался 400 (%v)", code, body["error"])
	}
}

// У проекта с креаторами стоимости в отчёте нет.
//
// Иначе рядом с начислениями появилось бы второе число про те же деньги,
// и они разъехались бы на первой правке тарифа.
func TestCreatorsProjectReportHasNoManualCost(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	if _, err := billing.NewRepo(pool).SaveTerms(ctx,
		billing.Terms{ProjectID: projectID, ProjectCost: 1_000_000}, creators[0]); err != nil {
		t.Fatalf("SaveTerms: %v", err)
	}
	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@u/video/5"}, map[int]int64{0: 10_000})

	rep, err := publications.NewService(publications.NewRepo(pool)).
		Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Cost != nil || rep.CostPer1000 != nil {
		t.Errorf("у проекта с креаторами в отчёте появилась своя стоимость: cost=%v cpv=%v",
			rep.Cost, rep.CostPer1000)
	}
}

// В проекте с креаторами не заводится ни одной выкладки без креатора.
//
// CHECK «NULL только у brand_turnkey» поставить нельзя — он не умеет
// смотреть в соседнюю таблицу. Правило держит Repo, и вот проверка, что
// держит.
func TestCreatorsProjectHasNoOwnerlessPublications(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID: projectID,
		Dates:     []time.Time{pubDay(1)},
		CreatedBy: creators[0],
	}); !errors.Is(err, publications.ErrNothingToCreate) {
		t.Errorf("пачка без креаторов в проекте с креаторами: ждали ErrNothingToCreate, got %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM project_publications p
JOIN projects pr ON pr.id = p.project_id
WHERE pr.kind = 'creators_turnkey' AND p.creator_user_id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count ownerless: %v", err)
	}
	if n != 0 {
		t.Errorf("в проектах с креаторами нашлось %d выкладок без владельца", n)
	}
}
