package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/orders"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// namedCreator — дать креатору профиль с именем и ссылками на аккаунты.
func namedCreator(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, name, links string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO specialist_profiles (user_id, display_name, is_published, social_links)
VALUES ($1, $2, TRUE, $3::jsonb)
ON CONFLICT (user_id) DO UPDATE
   SET display_name = EXCLUDED.display_name, social_links = EXCLUDED.social_links`,
		id, name, links); err != nil {
		t.Fatalf("profile for %s: %v", name, err)
	}
}

// ---- ТЕСТ: имена вместо uuid ----

// В выдаче был только creator_user_id, и в ростере читалось
// «Креатор 3f2a91b8». Имя живёт в профиле и должно доезжать до всех
// экранов, где креатор упомянут.
func TestOutputCarriesCreatorNames(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	namedCreator(t, pool, creators[0], "Анастасия Первая",
		`{"tiktok":"https://tiktok.com/@nastya","behance":"https://be.net/nastya"}`)
	namedCreator(t, pool, creators[1], "Андрей Второй", `{}`)

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: creators,
		Dates:          []time.Time{pubDay(1)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	t.Run("список выкладок", func(t *testing.T) {
		items, err := svc.ListForManager(ctx, pid)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("want 2 publications, got %d", len(items))
		}
		for _, p := range items {
			if p.CreatorName == "" {
				t.Errorf("выкладка %s без имени креатора", p.ID)
			}
		}
	})

	t.Run("состав проекта с именами и ссылками", func(t *testing.T) {
		roster, err := svc.ProjectCreators(ctx, pid)
		if err != nil {
			t.Fatalf("roster: %v", err)
		}
		if len(roster) != 2 {
			t.Fatalf("want 2 in roster, got %d", len(roster))
		}
		byID := map[uuid.UUID]publications.Person{}
		for _, p := range roster {
			byID[p.UserID] = p
		}
		first := byID[creators[0]]
		if first.Name != "Анастасия Первая" {
			t.Errorf("имя: %q", first.Name)
		}
		if first.AccountLinks["tiktok"] == "" {
			t.Errorf("ссылка на TikTok потерялась: %v", first.AccountLinks)
		}
		// behance — не наша площадка, в проекте ей делать нечего.
		if _, ok := first.AccountLinks["behance"]; ok {
			t.Errorf("в ссылках проекта оказалась чужая площадка: %v", first.AccountLinks)
		}
		if byID[creators[1]].AccountLinks != nil {
			t.Errorf("пустой social_links должен давать nil, а не пустую карту")
		}
	})
}

// ---- ТЕСТ: «мои проекты» у креатора ----

func TestCreatorProjectsList(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	// У первого две выкладки, одна из них просрочена; у второго — одна.
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-3), pubDay(5)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[1]},
		Dates:          []time.Time{pubDay(7)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("batch 2: %v", err)
	}

	got, err := svc.CreatorProjects(ctx, creators[0])
	if err != nil {
		t.Fatalf("creator projects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 project, got %d", len(got))
	}
	p := got[0]
	if p.ProjectID != pid {
		t.Errorf("не тот проект: %s", p.ProjectID)
	}
	// Счётчики — только по своим выкладкам: чужих креатор не видит нигде.
	if p.Total != 2 || p.Open != 2 {
		t.Errorf("счётчики считают чужие выкладки: total=%d open=%d", p.Total, p.Open)
	}
	if p.Overdue != 1 {
		t.Errorf("просрочка: want 1, got %d", p.Overdue)
	}
	if p.NextDueDate == nil {
		t.Errorf("не посчитан ближайший срок")
	}

	// Выбывший из состава проект в списке не видит: доступа к нему больше
	// нет, и строка вела бы на 404.
	if err := svc.RemoveCreator(ctx, pid, creators[0]); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got, err = svc.CreatorProjects(ctx, creators[0])
	if err != nil {
		t.Fatalf("creator projects after removal: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("выбывший креатор всё ещё видит проект: %+v", got)
	}
}

// ---- ТЕСТ: материалы и их аудитория ----

func TestMaterialsAudience(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	forCreators, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialDoc, Title: "Бренд-гайд",
		URL: "https://cdn.example.com/guide.pdf", CreatedBy: creators[0],
	})
	if err != nil {
		t.Fatalf("add creators material: %v", err)
	}
	if forCreators.Audience != publications.AudienceCreators {
		t.Errorf("аудитория по умолчанию должна быть creators, got %s", forCreators.Audience)
	}
	if _, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialLink, Title: "Готовый ролик",
		URL:      "https://disk.example.com/final.mp4",
		Audience: publications.AudienceClient, CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("add client material: %v", err)
	}

	// Клиент не видит обучение креаторов — это прямое требование.
	client, err := svc.ListMaterials(ctx, pid, publications.AudienceClient)
	if err != nil {
		t.Fatalf("list client: %v", err)
	}
	if len(client) != 1 || client[0].Title != "Готовый ролик" {
		t.Errorf("клиенту видно лишнее: %+v", client)
	}

	creatorSide, err := svc.ListMaterials(ctx, pid, publications.AudienceCreators)
	if err != nil {
		t.Fatalf("list creators: %v", err)
	}
	if len(creatorSide) != 1 || creatorSide[0].Title != "Бренд-гайд" {
		t.Errorf("креатору видно лишнее: %+v", creatorSide)
	}

	all, err := svc.ListMaterials(ctx, pid, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("менеджеру должно быть видно оба: %+v", all)
	}
	// Порядок задаётся при добавлении, а не случайным порядком строк.
	if all[0].SortOrder != 0 || all[1].SortOrder != 1 {
		t.Errorf("порядок материалов: %d, %d", all[0].SortOrder, all[1].SortOrder)
	}
}

func TestMaterialValidation(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	cases := []struct {
		name string
		in   publications.AddMaterialInput
	}{
		{"javascript-ссылка", publications.AddMaterialInput{Kind: "link", Title: "клик", URL: "javascript:alert(1)"}},
		{"неизвестный вид", publications.AddMaterialInput{Kind: "archive", Title: "архив", URL: "https://e.com/a.zip"}},
		{"без названия", publications.AddMaterialInput{Kind: "doc", Title: "  ", URL: "https://e.com/a.pdf"}},
		{"чужая аудитория", publications.AddMaterialInput{Kind: "doc", Title: "док", URL: "https://e.com/a.pdf", Audience: "everyone"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.ProjectID, in.CreatedBy = pid, creators[0]
			if _, err := svc.AddMaterial(ctx, in); !errors.Is(err, publications.ErrInvalidInput) {
				t.Errorf("want ErrInvalidInput, got %v", err)
			}
		})
	}
}

// Материал чужого проекта по своему id не удаляется: знание id не даёт
// права на удаление.
func TestDeleteMaterialScopedToProject(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	otherPID, _, cleanup2 := setupCreatorsProject(t, pool)
	defer cleanup2()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	m, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialDoc, Title: "Гайд",
		URL: "https://cdn.example.com/g.pdf", CreatedBy: creators[0],
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := svc.DeleteMaterial(ctx, otherPID, m.ID, creators[0]); !errors.Is(err, publications.ErrNotFound) {
		t.Errorf("удаление из чужого проекта: want ErrNotFound, got %v", err)
	}
	if err := svc.DeleteMaterial(ctx, pid, m.ID, creators[0]); err != nil {
		t.Errorf("удаление из своего проекта: %v", err)
	}
}

// ---- ТЕСТ: автопинг ----

// Тумблер должен гасить рассылку, а не просто лежать в базе.
func TestAutopingSuppressesReminders(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	// Одна выкладка на сегодня (due_today) и одна просроченная (overdue).
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0), pubDay(-2)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	prefs, err := svc.ReminderPrefs(ctx, pid)
	if err != nil {
		t.Fatalf("prefs: %v", err)
	}
	if !prefs.DueToday || !prefs.Overdue || !prefs.Incomplete || !prefs.ManagerDigest {
		t.Fatalf("проект без настроек должен пинговаться полностью: %+v", prefs)
	}

	now := time.Now().UTC()
	full, err := svc.RunReminders(ctx, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if full.Sent != 2 {
		t.Fatalf("ожидалось два напоминания, ушло %d (%+v)", full.Sent, full)
	}
	if full.Digests != 1 {
		t.Errorf("сводка менеджерам не ушла: %+v", full)
	}

	// Выключаем «утром в день выкладки» и «сводку». Журнал чистим — иначе
	// второй проход промолчит из-за дедупа, а не из-за настроек.
	prefs.DueToday = false
	prefs.ManagerDigest = false
	if _, err := svc.SaveReminderPrefs(ctx, prefs, creators[0]); err != nil {
		t.Fatalf("save prefs: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notification_log WHERE subject_id IN (
        SELECT id FROM project_publications WHERE project_id = $1) OR subject_id = $1`, pid); err != nil {
		t.Fatalf("clear log: %v", err)
	}

	limited, err := svc.RunReminders(ctx, now)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if limited.Sent != 1 {
		t.Errorf("должно остаться одно напоминание о просрочке, ушло %d (%+v)", limited.Sent, limited)
	}
	if limited.Digests != 0 {
		t.Errorf("сводка выключена, но ушла: %+v", limited)
	}

	// Настройки читаются обратно — тумблер не должен «отскакивать».
	got, err := svc.ReminderPrefs(ctx, pid)
	if err != nil {
		t.Fatalf("prefs after save: %v", err)
	}
	if got.DueToday || got.ManagerDigest {
		t.Errorf("выключенное вернулось включённым: %+v", got)
	}
	if !got.Overdue || !got.Incomplete {
		t.Errorf("нетронутые тумблеры выключились сами: %+v", got)
	}
}

// ---- ТЕСТ: библиотека чеклистов ----

func TestChecklistTemplatesListed(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	var tplID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO checklist_templates (name, description, version) VALUES ($1, 'для теста', 3)
RETURNING id`, "UGC-ролик "+uuid.NewString()).Scan(&tplID); err != nil {
		t.Fatalf("create template: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tplID)

	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `
INSERT INTO checklist_template_items (template_id, text, is_required, sort_order)
VALUES ($1, 'пункт', TRUE, $2)`, tplID, i); err != nil {
			t.Fatalf("create item: %v", err)
		}
	}

	items, err := svc.ChecklistTemplates(ctx)
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	var found *publications.ChecklistTemplate
	for i := range items {
		if items[i].ID == tplID {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatalf("шаблон не попал в библиотеку")
	}
	if found.ItemsCount != 3 {
		t.Errorf("пунктов: want 3, got %d", found.ItemsCount)
	}
	if found.Version != 3 {
		t.Errorf("версия: %d", found.Version)
	}

	// Выключенный шаблон подключить нельзя, значит и показывать незачем.
	if _, err := pool.Exec(ctx,
		`UPDATE checklist_templates SET is_active = FALSE WHERE id = $1`, tplID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	items, err = svc.ChecklistTemplates(ctx)
	if err != nil {
		t.Fatalf("templates 2: %v", err)
	}
	for _, it := range items {
		if it.ID == tplID {
			t.Errorf("выключенный шаблон остался в библиотеке")
		}
	}
}

// ---- ТЕСТ: креатор читает свою занятость ----

// Запись была, чтения не было: креатор отмечал месяц и не видел, что
// отметил. Не отмеченный месяц в выдачу не попадает — «не отмечал» и
// «занят» разные вещи.
func TestCreatorReadsOwnAvailability(t *testing.T) {
	pool := integration.Pool(t)
	_, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := orders.NewService(orders.NewRepo(pool))

	thisMonth := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	next := thisMonth.AddDate(0, 1, 0)

	if err := svc.SetAvailability(ctx, creators[0], next, false); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := svc.MyAvailability(ctx, creators[0], 12)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("должен вернуться ровно отмеченный месяц, вернулось %d: %+v", len(got), got)
	}
	if !got[0].Month.Equal(next) || got[0].IsAvailable {
		t.Errorf("не тот месяц или не та занятость: %+v", got[0])
	}

	// Чужую занятость по своему запросу не видно.
	other, err := svc.MyAvailability(ctx, creators[1], 12)
	if err != nil {
		t.Fatalf("read other: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("сосед видит чужие отметки: %+v", other)
	}
}

// ---- ТЕСТ: карточка проекта у креатора ----

// Верх страницы выкладок раньше собирался из списка выкладок, а брифа и
// месячного плана там взять было неоткуда.
func TestCreatorProjectCard(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	managerID := assignManager(t, pool, pid)
	namedCreator(t, pool, managerID, "Мария Менеджер", `{}`)
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET notes = 'Вертикальные ролики про корм', monthly_plan = 12,
		        draft_required = TRUE WHERE id = $1`, pid); err != nil {
		t.Fatalf("project fields: %v", err)
	}

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-1), pubDay(3)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	card, err := svc.CreatorProjectCard(ctx, pid, creators[0])
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if card.Brief != "Вертикальные ролики про корм" {
		t.Errorf("бриф: %q", card.Brief)
	}
	if card.MonthlyPlan == nil || *card.MonthlyPlan != 12 {
		t.Errorf("месячный план: %v", card.MonthlyPlan)
	}
	if !card.DraftRequired {
		t.Errorf("этап черновика не доехал")
	}
	if card.Manager == nil || card.Manager.Name != "Мария Менеджер" {
		t.Errorf("менеджер: %+v", card.Manager)
	}
	if len(card.Platforms) != 5 {
		t.Errorf("площадок должно быть пять: %v", card.Platforms)
	}
	// Счётчики те же, что в списке проектов: две выкладки, обе не сданы,
	// одна просрочена.
	if card.Total != 2 || card.Open != 2 || card.Overdue != 1 {
		t.Errorf("счётчики: total=%d open=%d overdue=%d", card.Total, card.Open, card.Overdue)
	}

	// Посторонний карточку не получает — и «нет проекта», а не «нельзя».
	if _, err := svc.CreatorProjectCard(ctx, pid, uuid.New()); !errors.Is(err, publications.ErrNotFound) {
		t.Errorf("посторонний получил карточку: %v", err)
	}
}

// ---- ТЕСТ: версия подключённого чек-листа ----

// В макете «Шаблон «UGC-ролик, 5 площадок» · v3 … в библиотеке вышла v4».
// Версия шаблона живёт колонкой в его же строке и переписывается при
// правке библиотеки, поэтому подключённую версию надо запоминать снимком.
func TestChecklistSnapshotRemembersVersion(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	// Чек-лист не подключён — шапки нет.
	meta, err := svc.ChecklistMeta(ctx, pid)
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if meta != nil {
		t.Errorf("шапка появилась до подключения: %+v", meta)
	}

	var tplID uuid.UUID
	name := "UGC-ролик " + uuid.NewString()
	if err := pool.QueryRow(ctx, `
INSERT INTO checklist_templates (name, version) VALUES ($1, 3) RETURNING id`, name).
		Scan(&tplID); err != nil {
		t.Fatalf("template: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tplID)
	if _, err := pool.Exec(ctx, `
INSERT INTO checklist_template_items (template_id, text, is_required, sort_order)
VALUES ($1, 'обложка', TRUE, 0)`, tplID); err != nil {
		t.Fatalf("item: %v", err)
	}

	if _, err := svc.SnapshotChecklist(ctx, pid, tplID, creators[0]); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	meta, err = svc.ChecklistMeta(ctx, pid)
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if meta == nil || meta.TemplateName != name || meta.TemplateVersion != 3 {
		t.Fatalf("шапка: %+v", meta)
	}
	if meta.LatestVersion != 3 {
		t.Errorf("пока обновлений нет, версии совпадают: %d", meta.LatestVersion)
	}

	// В библиотеке вышла новая версия — подключённая остаётся прежней.
	if _, err := pool.Exec(ctx,
		`UPDATE checklist_templates SET version = 4 WHERE id = $1`, tplID); err != nil {
		t.Fatalf("bump: %v", err)
	}
	meta, err = svc.ChecklistMeta(ctx, pid)
	if err != nil {
		t.Fatalf("meta 2: %v", err)
	}
	if meta.TemplateVersion != 3 {
		t.Errorf("подключённая версия переписалась вслед за библиотекой: %d", meta.TemplateVersion)
	}
	if meta.LatestVersion != 4 {
		t.Errorf("новая версия в библиотеке не видна: %d", meta.LatestVersion)
	}
}
