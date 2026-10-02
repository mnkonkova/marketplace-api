package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// setupCreatorsProject — проект вида creators_turnkey с двумя креаторами
// в составе. Опирается на setupPipelineAndProject из projects_test.go:
// проект всё равно создаётся из воронки, а kind переключается отдельно.
func setupCreatorsProject(t *testing.T, pool *pgxpool.Pool) (projectID uuid.UUID, creators []uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	_, _, projectID, baseCleanup := setupPipelineAndProject(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE projects SET kind = 'creators_turnkey' WHERE id = $1`, projectID); err != nil {
		t.Fatalf("set project kind: %v", err)
	}

	creators = make([]uuid.UUID, 0, 2)
	for i := 0; i < 2; i++ {
		var id uuid.UUID
		email := "creator-" + uuid.NewString() + "@example.com"
		if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`, email).Scan(&id); err != nil {
			t.Fatalf("create creator: %v", err)
		}
		creators = append(creators, id)
	}

	repo := publications.NewRepo(pool)
	for _, c := range creators {
		if err := repo.AddCreator(ctx, projectID, c, creators[0]); err != nil {
			t.Fatalf("add creator: %v", err)
		}
	}

	cleanup = func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, projectID)
		baseCleanup()
		for _, c := range creators {
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, c)
		}
	}
	return projectID, creators, cleanup
}

func pubDay(offset int) time.Time {
	return time.Now().UTC().AddDate(0, 0, offset).Truncate(24 * time.Hour)
}

// Пачка — основной путь менеджера. Проверяем и количество, и что каждая
// выкладка привязана к своему креатору: перепутать их местами — ровно та
// ошибка, из-за которой чистят шестьдесят строк руками.
func TestCreateBatchByScheme(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) // вторник
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	res, err := svc.CreateBatchByScheme(ctx, projectID, creators,
		publications.SchemeTueThu, from, to, 0, 0, creators[0])
	if err != nil {
		t.Fatalf("CreateBatchByScheme: %v", err)
	}

	// Сентябрь 2026: вторников и четвергов — 9.
	const datesPerCreator = 9
	if want := datesPerCreator * len(creators); res.Created != want {
		t.Fatalf("создано %d выкладок, ожидалось %d", res.Created, want)
	}

	perCreator := map[uuid.UUID]int{}
	for _, p := range res.Items {
		if p.CreatorUserID == nil {
			t.Fatalf("у выкладки проекта с креаторами нет владельца: %s", p.ID)
		}
		perCreator[*p.CreatorUserID]++
		if p.Status != publications.StatusPlanned {
			t.Errorf("новая выкладка должна быть planned, а она %s", p.Status)
		}
		if p.BatchID == nil || *p.BatchID != res.BatchID {
			t.Error("выкладка не помечена id пачки — отменить её пачкой будет нечем")
		}
	}
	for _, c := range creators {
		if perCreator[c] != datesPerCreator {
			t.Errorf("у креатора %s %d выкладок, ожидалось %d", c, perCreator[c], datesPerCreator)
		}
	}
}

func TestCreateBatchRejectsOutsiderAndWrongKind(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))

	// Чужой креатор — не в составе проекта.
	outsider := uuid.New()
	_, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{outsider},
		Dates:          []time.Time{pubDay(1)},
		CreatedBy:      creators[0],
	})
	if !errors.Is(err, publications.ErrCreatorNotInProject) {
		t.Errorf("чужому креатору выкладку заводить нельзя: got %v", err)
	}

	// Проект другого вида: выкладки бывают только у creators_turnkey.
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET kind = 'production_turnkey' WHERE id = $1`, projectID); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	_, err = svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(1)},
		CreatedBy:      creators[0],
	})
	if !errors.Is(err, publications.ErrNotCreatorsProject) {
		t.Errorf("у продакшна выкладок нет: got %v", err)
	}
}

// Отмена пачкой — вторая половина требования М2. Но чужую работу она
// стирать не должна: выкладка со сданными ссылками остаётся.
func TestCancelBatchKeepsSubmitted(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0), pubDay(1), pubDay(2)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	// По одной выкладке креатор уже отчитался.
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/1"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	cancelled, err := svc.CancelBatch(ctx, projectID, res.BatchID)
	if err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}
	if cancelled != 2 {
		t.Errorf("отменено %d, ожидалось 2 (третья уже сдана)", cancelled)
	}

	kept, err := svc.Get(ctx, res.Items[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if kept.Status == publications.StatusCancelled {
		t.Error("сданную выкладку отмена пачкой стёрла — работа креатора потеряна")
	}
}

// Выкладка закрывается на пяти ссылках; одна-четыре — сдана частично.
// Досылать площадки можно позже, и это не должно плодить дубли.
func TestSubmitLinksPartialThenDone(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	got, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs: []string{
			"https://www.tiktok.com/@u/video/123?is_from_webapp=1",
			"https://youtu.be/dQw4w9WgXcQ",
			"https://www.instagram.com/reel/C8xYzAbCdEf/",
		},
	})
	if err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	if got.Status != publications.StatusPartial {
		t.Errorf("три площадки из пяти — это partial, а не %s", got.Status)
	}
	if missing := got.MissingPlatforms(); len(missing) != 2 {
		t.Errorf("не хватает %v, ожидалось две площадки", missing)
	}

	// Дослали остальные две — плюс повтор уже сданной ссылки.
	got, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs: []string{
			"https://vk.com/clip-99_11",
			"https://likee.video/v/AbCdEf",
			"https://www.tiktok.com/@u/video/123",
		},
	})
	if err != nil {
		t.Fatalf("SubmitLinks (добор): %v", err)
	}
	if got.Status != publications.StatusDone {
		t.Errorf("пять площадок — это done, а не %s", got.Status)
	}
	if len(got.Links) != 5 {
		t.Errorf("ссылок %d, ожидалось 5: повторная сдача не должна плодить строки", len(got.Links))
	}
}

func TestSubmitLinksRejectsForeignCreatorAndGarbage(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	// Сдаёт другой креатор проекта — тоже нельзя.
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[1],
		URLs:          []string{"https://www.tiktok.com/@u/video/1"},
	})
	if !errors.Is(err, publications.ErrForbidden) {
		t.Errorf("чужую выкладку сдавать нельзя: got %v", err)
	}

	// Две ссылки на одну площадку — иначе одна молча перезапишет другую.
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs: []string{
			"https://www.tiktok.com/@u/video/1",
			"https://www.tiktok.com/@u/video/2",
		},
	})
	if !errors.Is(err, publications.ErrDuplicatePlatform) {
		t.Errorf("дубль площадки: got %v", err)
	}

	// Не из пятёрки.
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://rutube.ru/video/abc/"},
	})
	if !errors.Is(err, publications.ErrUnknownPlatform) {
		t.Errorf("чужая площадка: got %v", err)
	}
}

// Обязательные пункты чеклиста блокируют сдачу на бэке, а не только в UI:
// иначе проверка обходится прямым запросом к API.
func TestChecklistBlocksSubmit(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	tplID := createChecklistTemplate(t, pool)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tplID) }()

	n, err := svc.SnapshotChecklist(ctx, projectID, tplID, creators[0])
	if err != nil {
		t.Fatalf("SnapshotChecklist: %v", err)
	}
	if n != 3 {
		t.Fatalf("скопировано %d пунктов, ожидалось 3", n)
	}
	items, err := svc.ProjectChecklist(ctx, projectID)
	if err != nil {
		t.Fatalf("ProjectChecklist: %v", err)
	}

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	// Ничего не отмечено — сдача не проходит.
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/1"},
	})
	if !errors.Is(err, publications.ErrChecklistIncomplete) {
		t.Fatalf("необязательная проверка чеклиста: got %v", err)
	}

	// Отмечаем то, что относится к TikTok: общий пункт и пункт площадки.
	// Пункт для YouTube не требуется — эту площадку сейчас не сдают.
	checked := make([]uuid.UUID, 0, 2)
	for _, it := range items {
		if it.IsRequired && it.AppliesTo(publications.PlatformTikTok) {
			checked = append(checked, it.ID)
		}
	}
	if len(checked) != 2 {
		t.Fatalf("к TikTok относится %d обязательных пунктов, ожидалось 2", len(checked))
	}

	got, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID:  pubID,
		ActorUserID:    creators[0],
		URLs:           []string{"https://www.tiktok.com/@u/video/1"},
		CheckedItemIDs: checked,
	})
	if err != nil {
		t.Fatalf("SubmitLinks с отмеченным чеклистом: %v", err)
	}
	if got.Status != publications.StatusPartial {
		t.Errorf("статус %s, ожидался partial", got.Status)
	}
}

// Правка библиотеки не меняет идущие проекты — тот же приём, что у воронок.
func TestChecklistSnapshotIsIndependentOfLibrary(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	tplID := createChecklistTemplate(t, pool)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tplID) }()

	if _, err := svc.SnapshotChecklist(ctx, projectID, tplID, creators[0]); err != nil {
		t.Fatalf("SnapshotChecklist: %v", err)
	}
	before, _ := svc.ProjectChecklist(ctx, projectID)

	// Библиотеку переписали: пункт удалили и добавили новый.
	if _, err := pool.Exec(ctx, `
DELETE FROM checklist_template_items WHERE template_id = $1 AND sort_order = 0`, tplID); err != nil {
		t.Fatalf("edit library: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO checklist_template_items (template_id, text, is_required, sort_order)
VALUES ($1, 'Новое требование', TRUE, 99)`, tplID); err != nil {
		t.Fatalf("add library item: %v", err)
	}

	after, err := svc.ProjectChecklist(ctx, projectID)
	if err != nil {
		t.Fatalf("ProjectChecklist: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("снимок проекта поехал за библиотекой: было %d, стало %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Text != after[i].Text {
			t.Errorf("пункт %d изменился: %q → %q", i, before[i].Text, after[i].Text)
		}
	}
}

// Креатор видит только свои выкладки. Это не косметика выдачи, а граница
// доступа: чужие строки не должны доезжать до ответа API.
func TestCreatorSeesOnlyOwnPublications(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators,
		Dates:          []time.Time{pubDay(0), pubDay(1)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	all, err := svc.ListForManager(ctx, projectID)
	if err != nil {
		t.Fatalf("ListForManager: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("менеджер видит %d выкладок, ожидалось 4", len(all))
	}

	mine, err := svc.ListForCreator(ctx, projectID, creators[0])
	if err != nil {
		t.Fatalf("ListForCreator: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("креатор видит %d выкладок, ожидалось 2", len(mine))
	}
	for _, p := range mine {
		if p.CreatorUserID == nil || *p.CreatorUserID != creators[0] {
			t.Errorf("в выдачу креатора попала чужая выкладка %s", p.ID)
		}
	}
}

// Просрочка вычисляется, а не хранится. И пока висит просьба о переносе,
// выкладка просроченной не считается — пинги по человеку, который уже
// предупредил, не идут.
func TestOverdueSuppressedByPendingDateRequest(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-3)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	got, err := svc.Get(ctx, pubID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Overdue {
		t.Fatal("выкладка с дедлайном три дня назад должна быть просроченной")
	}

	req, err := svc.RequestDateChange(ctx, pubID, creators[0], pubDay(5), "заболел, перенесу")
	if err != nil {
		t.Fatalf("RequestDateChange: %v", err)
	}

	got, err = svc.Get(ctx, pubID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Overdue {
		t.Error("пока просьба о переносе не рассмотрена, просрочки нет")
	}
	if got.PendingDateRequest == nil {
		t.Fatal("просьба должна приезжать вместе с выкладкой")
	}

	// Вторую открытую просьбу по той же выкладке заводить нельзя.
	if _, err := svc.RequestDateChange(ctx, pubID, creators[0], pubDay(6), "ещё раз"); !errors.Is(err, publications.ErrAlreadyRequested) {
		t.Errorf("вторая открытая просьба: got %v", err)
	}

	// Менеджер согласовал — дата уезжает вместе с решением.
	if err := svc.DecideDateRequest(ctx, req.ID, creators[0], true); err != nil {
		t.Fatalf("DecideDateRequest: %v", err)
	}
	got, err = svc.Get(ctx, pubID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.DueDate.Equal(pubDay(5)) {
		t.Errorf("после согласования дедлайн %s, ожидался %s",
			got.DueDate.Format("2006-01-02"), pubDay(5).Format("2006-01-02"))
	}
	if got.Overdue {
		t.Error("перенесённая в будущее выкладка не просрочена")
	}
}

// Закрытие неполной выкладки — исключение, и у него обязан быть автор и
// причина. Без причины БД не пропустит, но ошибка должна быть внятной.
func TestCloseManuallyRequiresReason(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	if _, err := svc.CloseManually(ctx, publications.CloseManuallyInput{
		PublicationID: pubID,
		ManagerUserID: creators[0],
		Reason:        "   ",
	}); !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("закрытие без причины: got %v", err)
	}

	got, err := svc.CloseManually(ctx, publications.CloseManuallyInput{
		PublicationID: pubID,
		ManagerUserID: creators[0],
		Reason:        "клиент согласовал четыре площадки, Likee не требуется",
	})
	if err != nil {
		t.Fatalf("CloseManually: %v", err)
	}
	if got.Status != publications.StatusClosedManually {
		t.Errorf("статус %s, ожидался closed_manually", got.Status)
	}
	if got.ClosedBy == nil || got.CloseReason == "" {
		t.Error("у закрытия должен остаться автор и причина")
	}

	// По закрытой выкладке досылать ссылки уже нельзя.
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/9"},
	}); !errors.Is(err, publications.ErrPublicationClosed) {
		t.Errorf("сдача в закрытую выкладку: got %v", err)
	}
}

// createChecklistTemplate — шаблон из трёх пунктов: общий, для TikTok
// и для YouTube. Возвращает id шаблона.
func createChecklistTemplate(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var tplID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO checklist_templates (name) VALUES ($1) RETURNING id`,
		"Чеклист "+uuid.NewString()).Scan(&tplID); err != nil {
		t.Fatalf("create checklist template: %v", err)
	}
	items := []struct {
		text     string
		platform *string
		order    int
	}{
		{"Упомянуть бренд в первые 3 секунды", nil, 0},
		{"Хештег в описании", strPtr("tiktok"), 1},
		{"Ссылка в описании", strPtr("youtube"), 2},
	}
	for _, it := range items {
		if _, err := pool.Exec(ctx, `
INSERT INTO checklist_template_items (template_id, text, platform, is_required, sort_order)
VALUES ($1, $2, $3, TRUE, $4)`, tplID, it.text, it.platform, it.order); err != nil {
			t.Fatalf("create checklist item: %v", err)
		}
	}
	return tplID
}

func strPtr(s string) *string { return &s }

// В креаторы проекта можно добавить только специалиста. Иначе заказчик,
// менеджер или админ, добавленный «креатором», получил бы доступ к
// выкладкам проекта наравне с исполнителями.
func TestAddCreatorRejectsNonSpecialists(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)

	mk := func(kind string, manager, admin, active bool) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_manager, is_admin, is_active,
                   is_approved, email_verified_at)
VALUES ($1, 'x', $2, $3, $4, $5, TRUE, now()) RETURNING id`,
			"addcreator-"+uuid.NewString()+"@example.com", kind, manager, admin, active).Scan(&id); err != nil {
			t.Fatalf("create user: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id) })
		return id
	}

	cases := []struct {
		name string
		id   uuid.UUID
	}{
		{"заказчик", mk("client", false, false, true)},
		{"менеджер", mk("specialist", true, false, true)},
		{"админ", mk("specialist", false, true, true)},
		{"отключённый аккаунт", mk("specialist", false, false, false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := repo.AddCreator(ctx, projectID, c.id, creators[0])
			if !errors.Is(err, publications.ErrNotACreator) {
				t.Errorf("got %v, want ErrNotACreator", err)
			}
		})
	}

	// Специалист и «оба» — можно.
	for _, kind := range []string{"specialist", "both"} {
		if err := repo.AddCreator(ctx, projectID, mk(kind, false, false, true), creators[0]); err != nil {
			t.Errorf("kind=%s должен приниматься, а got %v", kind, err)
		}
	}

	// Несуществующего — нет.
	if err := repo.AddCreator(ctx, projectID, uuid.New(), creators[0]); !errors.Is(err, publications.ErrNotFound) {
		t.Errorf("несуществующий пользователь: got %v, want ErrNotFound", err)
	}
}

// Прогресс проекта с креаторами считается по выкладкам.
//
// Шагов у такого проекта нет вовсе, и взвешенный прогресс по шагам всегда
// давал ноль: проект с половиной сданных роликов выглядел не начатым — и
// у менеджера в списке, и у заказчика на карточке.
func TestCreatorsProjectProgressCountsPublications(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	repo := projects.NewRepo(pool)

	// Четыре выкладки, две сданы.
	seedPublicationViews(t, pid, creators[0], pubDay(0), "prg1", 1000, true)
	seedPublicationViews(t, pid, creators[0], pubDay(1), "prg2", 1000, true)
	seedPublicationViews(t, pid, creators[0], pubDay(2), "prg3", 0, false)
	seedPublicationViews(t, pid, creators[0], pubDay(3), "prg4", 0, false)

	got, err := repo.PublicationProgress(ctx, []uuid.UUID{pid})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if pct := got[pid]; pct < 49 || pct > 51 {
		t.Errorf("две выкладки из четырёх — это половина, получили %.1f%%", pct)
	}
}

// Проект без выкладок в карту не попадает: делить на ноль нечего, и
// «0%» здесь означало бы «ничего не сдано», а не «плана ещё нет».
func TestPublicationProgressSkipsProjectsWithoutPublications(t *testing.T) {
	pool := integration.Pool(t)
	pid, _, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	got, err := projects.NewRepo(pool).PublicationProgress(context.Background(), []uuid.UUID{pid})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if _, ok := got[pid]; ok {
		t.Error("проект без выкладок не должен получать долю закрытых")
	}
}

// ---- библиотека чеклистов ----

// Шаблон правится только новой версией: прежняя гасится, а проекты,
// снявшие с неё снимок, не меняются. Правка на месте означала бы, что
// креатор отмечал одно, а спросят с него другое.
func TestChecklistTemplateIsReplacedNotEdited(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	name := "Чеклист " + uuid.NewString()[:8]
	v1, err := svc.SaveChecklistTemplate(ctx, uuid.Nil, uuid.Nil, name, "первый", []publications.ChecklistTemplateItem{
		{Text: "Товар в кадре первые 3 секунды", IsRequired: true},
		{Text: "Ссылка в закрепе", Platform: "tiktok", IsRequired: false},
	})
	if err != nil {
		t.Fatalf("save v1: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM checklist_templates WHERE name = $1`, name)
	})
	if v1.Version != 1 || len(v1.Items) != 2 {
		t.Fatalf("первая версия: version=%d items=%d", v1.Version, len(v1.Items))
	}

	v2, err := svc.SaveChecklistTemplate(ctx, uuid.Nil, v1.ID, name, "второй", []publications.ChecklistTemplateItem{
		{Text: "Товар в кадре первые 3 секунды", IsRequired: true},
	})
	if err != nil {
		t.Fatalf("save v2: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("вторая версия должна быть 2, а не %d", v2.Version)
	}
	if v2.ID == v1.ID {
		t.Error("новая версия — новая запись, а не переписанная старая")
	}

	// Старая версия из библиотеки ушла, новая осталась — ровно одна.
	lib, err := svc.ChecklistTemplates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen int
	for _, tpl := range lib {
		if tpl.Name == name {
			seen++
			if tpl.ID != v2.ID {
				t.Error("в библиотеке осталась погашенная версия")
			}
		}
	}
	if seen != 1 {
		t.Errorf("шаблон в библиотеке должен быть один, нашлось %d", seen)
	}

	// А прочитать старую по id всё ещё можно: на неё ссылаются снимки.
	if old, err := svc.ChecklistTemplate(ctx, v1.ID); err != nil || len(old.Items) != 2 {
		t.Errorf("старая версия должна остаться читаемой: %v", err)
	}
}

// Шаблон без пунктов ничего не проверяет, и заводить его нельзя.
func TestChecklistTemplateNeedsNameAndItems(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	if _, err := svc.SaveChecklistTemplate(ctx, uuid.Nil, uuid.Nil, "  ", "", []publications.ChecklistTemplateItem{
		{Text: "пункт"},
	}); !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("пустое название: ожидали отказ, получили %v", err)
	}
	if _, err := svc.SaveChecklistTemplate(ctx, uuid.Nil, uuid.Nil, "Без пунктов", "",
		[]publications.ChecklistTemplateItem{{Text: "   "}}); !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("шаблон без пунктов: ожидали отказ, получили %v", err)
	}
	if _, err := svc.SaveChecklistTemplate(ctx, uuid.Nil, uuid.Nil, "Кривая площадка", "",
		[]publications.ChecklistTemplateItem{{Text: "пункт", Platform: "telegram"}}); !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("чужая площадка: ожидали отказ, получили %v", err)
	}
}

// ---- удаление креатора из состава ----

// Удаление мягкое: сданные выкладки и цифры в отчёте остаются.
//
// Иначе кнопка «убрать», нажатая по ошибке в середине месяца, стирала бы
// работу человека вместе с его строкой в отчёте — и восстановить её было
// бы нечем. Тихая часть здесь именно эта: интерфейс после удаления
// выглядит правильно в обоих случаях, а разница видна только в отчёте.
func TestRemoveCreatorKeepsDeliveredWork(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))
	pubID := seedPublicationViews(t, pid, creators[0], pubDay(0), "rm1", 120_000, true)

	if err := svc.RemoveCreator(ctx, pid, creators[0]); err != nil {
		t.Fatalf("remove creator: %v", err)
	}

	// Из состава ушёл.
	roster, err := svc.ProjectCreators(ctx, pid)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	for _, p := range roster {
		if p.UserID == creators[0] {
			t.Fatal("удалённый креатор остался в составе")
		}
	}

	// А выкладка и её ссылки на месте.
	var status string
	var links int
	if err := pool.QueryRow(ctx, `
SELECT p.status::text, (SELECT count(*) FROM publication_links l WHERE l.publication_id = p.id)
FROM project_publications p WHERE p.id = $1`, pubID).Scan(&status, &links); err != nil {
		t.Fatalf("выкладка исчезла вместе с человеком: %v", err)
	}
	if status != "done" || links != 5 {
		t.Errorf("сданная работа пострадала: status=%s links=%d", status, links)
	}

	// И в отчёте он остался — за месяц ему платят.
	rep, err := svc.Report(ctx, pid, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	var seen bool
	for _, row := range rep.ByCreator {
		if row.CreatorUserID == creators[0] {
			seen = true
			if row.Views != 120_000 {
				t.Errorf("просмотры удалённого креатора: %d", row.Views)
			}
		}
	}
	if !seen {
		t.Error("удалённый креатор пропал из отчёта — платить будет не за что")
	}
}

// Повторное удаление и удаление чужого — не молчаливый успех.
func TestRemoveCreatorTwiceIsAnError(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))
	if err := svc.RemoveCreator(ctx, pid, creators[0]); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := svc.RemoveCreator(ctx, pid, creators[0]); err == nil {
		t.Error("второе удаление должно сообщать, что удалять уже нечего")
	}
}

// ---- название ролика ----

// Название пишет креатор в момент сдачи — он единственный, кто знает,
// что снял.
//
// Тему выкладки задаёт дата, и пока ролик не вышел, названия у него нет.
// Но после выкладки дата перестаёт что-либо значить: в ленте заказчика
// двенадцать строк «Выкладка 07», и какая из них про распаковку, а какая
// про сравнение с конкурентом — не понять ни менеджеру, ни клиенту.
func TestCreatorNamesVideoOnSubmit(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	// У запланированной названия нет — и это нормально.
	if res.Items[0].Title != "" {
		t.Errorf("у запланированной выкладки взялось название: %q", res.Items[0].Title)
	}

	got, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		Title:         "Распаковка корма: первая реакция кота",
		URLs:          []string{"https://www.tiktok.com/@a/video/9001"},
	})
	if err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	if got.Title != "Распаковка корма: первая реакция кота" {
		t.Errorf("название не сохранилось: %q", got.Title)
	}

	// Досылая остальные площадки, поле можно не повторять — и записанное
	// не должно обнулиться.
	again, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.instagram.com/reel/C9xK2mLpQ7v/"},
	})
	if err != nil {
		t.Fatalf("SubmitLinks (досылка): %v", err)
	}
	if again.Title != "Распаковка корма: первая реакция кота" {
		t.Errorf("досылка затёрла название: %q", again.Title)
	}

	// А новое название заменяет прежнее: креатор вправе переименовать.
	renamed, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		Title:         "Кот и новый корм",
		URLs:          []string{"https://www.youtube.com/shorts/kQ2Vn8pLxJc"},
	})
	if err != nil {
		t.Fatalf("SubmitLinks (переименование): %v", err)
	}
	if renamed.Title != "Кот и новый корм" {
		t.Errorf("название не заменилось: %q", renamed.Title)
	}
}

// Слишком длинное название отклоняется словами, а не обрезается молча.
func TestVideoTitleLengthIsChecked(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(1)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		Title:         strings.Repeat("я", 121),
		URLs:          []string{"https://www.tiktok.com/@a/video/9002"},
	})
	if !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("ожидали отказ по длине названия, получили %v", err)
	}
}

// Чужую выкладку проверяем ДО похода в сеть.
//
// Разворачивание коротких ссылок ходит по адресу, который прислал
// человек. Пока проверка владельца жила в репозитории, она случалась
// ПОСЛЕ: ответ приходил 404, а запросы с нашего адреса уже уходили —
// любой залогиненный получал «слепой» исходящий GET чужими руками.
func TestSubmitLinksChecksOwnerBeforeNetwork(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	spy := &countingExpander{}
	svc = svc.WithURLExpander(spy)

	// Сдаёт НЕ владелец и присылает короткую ссылку — ту самую, ради
	// которой мы ходим в сеть.
	_, err = svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[1],
		URLs:          []string{"https://vt.tiktok.com/ZSbyuhrDf"},
	})
	if !errors.Is(err, publications.ErrForbidden) {
		t.Fatalf("чужая сдача: %v, ожидалось ErrForbidden", err)
	}
	if spy.calls != 0 {
		t.Errorf("в сеть сходили %d раз по чужому запросу — проверка прав опоздала", spy.calls)
	}
}

// countingExpander — считает походы в сеть, сам никуда не ходит.
type countingExpander struct{ calls int }

func (c *countingExpander) Expand(_ context.Context, raw string) (string, error) {
	c.calls++
	return raw, nil
}

// Пересдача ДРУГОГО адреса на ту же площадку обнуляет замеры.
//
// Строка ссылки при пересдаче остаётся та же (upsert по паре «выкладка
// и площадка»), а с ней оставался и весь ряд просмотров старого видео:
// накопительные цифры двух роликов склеивались в одну линию. Менеджер
// этот случай чистил, а сдача через окно — нет, и одно действие давало
// разный результат в зависимости от того, какой кнопкой сделано.
func TestResubmitWithNewURLResetsStats(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	submit := func(url string) {
		t.Helper()
		if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
			PublicationID: pubID, ActorUserID: creators[0], URLs: []string{url},
		}); err != nil {
			t.Fatalf("SubmitLinks %s: %v", url, err)
		}
	}
	countStats := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
WHERE l.publication_id = $1`, pubID).Scan(&n); err != nil {
			t.Fatalf("count stats: %v", err)
		}
		return n
	}

	submit("https://www.tiktok.com/@u/video/111")
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM publication_links WHERE publication_id = $1`, pubID).Scan(&linkID); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments)
VALUES ($1, CURRENT_DATE, 500000, 100, 10)`, linkID); err != nil {
		t.Fatalf("посеять замер: %v", err)
	}

	// Тот же адрес — замеры на месте: ролик тот же, его и правили.
	submit("https://www.tiktok.com/@u/video/111")
	if n := countStats(); n != 1 {
		t.Fatalf("после пересдачи того же адреса замеров %d, ожидался один", n)
	}

	// Другой адрес — другой ролик, прежние цифры к нему не относятся.
	submit("https://www.tiktok.com/@u/video/222")
	if n := countStats(); n != 0 {
		t.Errorf("после смены адреса осталось %d замеров — цифры старого ролика "+
			"приклеились к новому", n)
	}
}
