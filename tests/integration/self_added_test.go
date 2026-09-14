package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"

	"marketpclce/internal/auth"
	"marketpclce/internal/billing"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Креатор добавляет себе выкладку сам — чтобы добрать до ступени, когда
// план периода уже выполнен, а просмотров не хватает.
//
// Главное правило здесь одно: такая выкладка НЕ попадает в знаменатель
// недосдачи. Иначе кнопка работала бы против того, кто её нажал —
// добавил пять роликов, сдал два, потерял часть оклада за три, которых
// ему никто не поручал.

func mountSelfAdd(h *publications.Handler, actor uuid.UUID) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), actor)))
		})
	})
	r.Post("/me/creator/projects/{id}/publications", h.CreatorAddPublication)
	r.Get("/manager/projects/{id}/publications", h.ManagerList)
	return r
}

// selfAdded — выкладка, заведённая самим креатором и вышедшая в
// указанный день. Дата выхода ставится явно: период считается по факту
// выхода, а не по плановой дате.
func selfAdded(t *testing.T, pool *pgxpool.Pool, svc *publications.Service,
	pid, creator uuid.UUID, due, published time.Time, marker string, links []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	p, err := svc.AddSelfPublication(ctx, pid, creator, due, time.Now().UTC())
	if err != nil {
		t.Fatalf("самодобавленная выкладка: %v", err)
	}
	if !p.SelfAdded {
		t.Fatal("признак self_added не проставился при создании")
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: p.ID,
		ActorUserID:   creator,
		URLs:          links,
	}); err != nil {
		t.Fatalf("ссылки на самодобавленную выкладку: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET published_at = $2, submitted_at = $2
WHERE publication_id = $1`, p.ID, published); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}
	return p.ID
}

func oneLink(marker string) []string {
	return []string{"https://www.tiktok.com/@a/video/" + marker}
}

func fiveLinks(marker string) []string {
	return []string{
		"https://www.tiktok.com/@a/video/" + marker,
		"https://www.instagram.com/reel/" + marker + "/",
		"https://www.youtube.com/shorts/" + marker,
		"https://vk.com/clip-1_" + marker,
		"https://likee.video/@a/video/" + marker,
	}
}

// ГЛАВНЫЙ ТЕСТ. План из десяти сдан полностью, сверху креатор добавил
// себе три ролика и не закрыл их. Вычета быть не должно: он сделал всё,
// что ему поручали.
func TestSelfAddedDoesNotEnlargeDeductionDenominator(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	pubSvc := publications.NewService(publications.NewRepo(pool))
	billSvc := billing.NewService(billing.NewRepo(pool))
	if _, err := billSvc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	// Десять поручённых роликов, все сданы полностью.
	for i := 0; i < 10; i++ {
		publishOn(t, pool, pid, creators[0], today, "sad"+string(rune('a'+i)), 10_000)
	}
	// И три своих, ни одного не закрыл: только по одной площадке.
	for i := 0; i < 3; i++ {
		marker := "sfx" + string(rune('a'+i))
		selfAdded(t, pool, pubSvc, pid, creators[0],
			pubDay(300+i), today, marker, oneLink(marker))
	}

	rows, err := recalcCurrent(t, billSvc, pid)
	if err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	var mine *billing.Accrual
	for i := range rows {
		if rows[i].CreatorUserID == creators[0] {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatal("начисления креатора нет")
	}

	// Сторож: если самодобавленные не попали в период вовсе, тест
	// проверял бы пустоту. Всего роликов должно быть тринадцать.
	if mine.VideosPlanned != 13 {
		t.Fatalf("в периоде %d роликов, ожидалось 13 — тест не проверяет то, ради чего написан",
			mine.VideosPlanned)
	}
	if mine.VideosDelivered != 10 {
		t.Fatalf("сдано %d роликов, ожидалось 10", mine.VideosDelivered)
	}
	// И при этом никакого вычета: поручённое сдано полностью.
	if mine.Deduction != 0 {
		t.Errorf("вычет %d — самодобавленные попали в знаменатель недосдачи", mine.Deduction)
	}
	if mine.Salary != demoTerms(pid).SalaryPerMonth {
		t.Errorf("оклад %d, ожидался полный %d", mine.Salary, demoTerms(pid).SalaryPerMonth)
	}
}

// Настоящая недосдача по поручённым роликам вычет по-прежнему даёт:
// проверка того, что правило не отключило недосдачу вовсе.
func TestAssignedShortfallStillDeducts(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	pubSvc := publications.NewService(publications.NewRepo(pool))
	billSvc := billing.NewService(billing.NewRepo(pool))
	if _, err := billSvc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	// Два поручённых сданы, два — нет (вышли только на одной площадке).
	publishOn(t, pool, pid, creators[0], today, "asd1", 10_000)
	publishOn(t, pool, pid, creators[0], today, "asd2", 10_000)
	for i := 0; i < 2; i++ {
		marker := "asp" + string(rune('a'+i))
		pubID := seedPublicationViews(t, pid, creators[0], nextDue(), marker, 0, false)
		if _, err := pubSvc.SubmitLinks(ctx, publications.SubmitLinksInput{
			PublicationID: pubID,
			ActorUserID:   creators[0],
			URLs:          oneLink(marker),
		}); err != nil {
			t.Fatalf("частичная сдача: %v", err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE publication_links SET published_at = $2, submitted_at = $2
WHERE publication_id = $1`, pubID, today); err != nil {
			t.Fatalf("дата публикации: %v", err)
		}
	}
	// И один свой, тоже незакрытый: на вычет он влиять не должен.
	selfAdded(t, pool, pubSvc, pid, creators[0], pubDay(320), today, "asx", oneLink("asx"))

	rows, err := recalcCurrent(t, billSvc, pid)
	if err != nil {
		t.Fatalf("пересчёт: %v", err)
	}
	var mine *billing.Accrual
	for i := range rows {
		if rows[i].CreatorUserID == creators[0] {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatal("начисления креатора нет")
	}
	// Половина поручённого не сдана — половина оклада и вычитается.
	want := demoTerms(pid).SalaryPerMonth / 2
	if mine.Deduction != want {
		t.Errorf("вычет %d, ожидался %d (двое из четырёх поручённых)", mine.Deduction, want)
	}
}

// Просмотры самодобавленного ролика идут в период, ступени и бонус
// наравне с остальными: просмотры есть просмотры.
func TestSelfAddedViewsCountTowardsMoney(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	pubSvc := publications.NewService(publications.NewRepo(pool))
	billSvc := billing.NewService(billing.NewRepo(pool))
	if _, err := billSvc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("условия: %v", err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	publishOn(t, pool, pid, creators[0], today, "svm1", 100_000)

	before, err := recalcCurrent(t, billSvc, pid)
	if err != nil {
		t.Fatalf("пересчёт до: %v", err)
	}
	pubID := selfAdded(t, pool, pubSvc, pid, creators[0], pubDay(340), today, "svx", oneLink("svx"))
	setLinkViewsOn(t, pool, pubID, today, 200_000)

	after, err := recalcCurrent(t, billSvc, pid)
	if err != nil {
		t.Fatalf("пересчёт после: %v", err)
	}
	pick := func(items []billing.Accrual) billing.Accrual {
		for _, a := range items {
			if a.CreatorUserID == creators[0] {
				return a
			}
		}
		t.Fatal("начисления креатора нет")
		return billing.Accrual{}
	}
	b, a := pick(before), pick(after)
	if a.ViewsTotal != b.ViewsTotal+200_000 {
		t.Errorf("просмотры %d → %d, ожидался прирост на 200 000", b.ViewsTotal, a.ViewsTotal)
	}
	if a.ViewsBonus <= b.ViewsBonus {
		t.Errorf("бонус за просмотры не вырос: %d → %d", b.ViewsBonus, a.ViewsBonus)
	}
}

// Отказы: прошлое, подытоженный период, чужой проект, занятый день.
func TestSelfAddRefusals(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	pubSvc := publications.NewService(publications.NewRepo(pool))
	h := publications.NewHandler(pubSvc)
	srv := mountSelfAdd(h, creators[0])
	path := "/me/creator/projects/" + pid.String() + "/publications"

	t.Run("в прошлое нельзя", func(t *testing.T) {
		code, body := doJSON(t, srv, http.MethodPost, path,
			map[string]any{"due_date": pubDay(-1).Format("2006-01-02")})
		if code != http.StatusBadRequest {
			t.Fatalf("код %d, ожидался 400 (%v)", code, body)
		}
	})

	t.Run("дата не датой", func(t *testing.T) {
		code, body := doJSON(t, srv, http.MethodPost, path, map[string]any{"due_date": "завтра"})
		if code != http.StatusBadRequest || body["error"] != "bad_date" {
			t.Fatalf("код %d, ошибка %v — ожидался 400/bad_date", code, body["error"])
		}
	})

	t.Run("день уже занят", func(t *testing.T) {
		day := pubDay(350).Format("2006-01-02")
		if code, body := doJSON(t, srv, http.MethodPost, path, map[string]any{"due_date": day}); code != http.StatusCreated {
			t.Fatalf("первая выкладка: код %d (%v)", code, body)
		}
		code, body := doJSON(t, srv, http.MethodPost, path, map[string]any{"due_date": day})
		if code != http.StatusConflict || body["error"] != "day_taken" {
			t.Fatalf("код %d, ошибка %v — ожидался 409/day_taken, а не пятисотка из драйвера",
				code, body["error"])
		}
	})

	t.Run("чужой проект", func(t *testing.T) {
		other, _, cleanupOther := setupCreatorsProject(t, pool)
		defer cleanupOther()
		code, body := doJSON(t, srv, http.MethodPost,
			"/me/creator/projects/"+other.String()+"/publications",
			map[string]any{"due_date": pubDay(352).Format("2006-01-02")})
		if code != http.StatusNotFound {
			t.Fatalf("код %d, ожидался 404 (%v)", code, body)
		}
	})

	t.Run("подытоженный период", func(t *testing.T) {
		today := time.Now().UTC().Truncate(24 * time.Hour)
		publishOn(t, pool, pid, creators[0], today, "slk1", 1_000)

		billSvc := billing.NewService(billing.NewRepo(pool))
		p, err := billSvc.Period(ctx, pid, 1, today)
		if err != nil {
			t.Fatalf("период: %v", err)
		}
		if _, err := billSvc.LockPeriod(ctx, p, nil, today, today); err != nil {
			t.Fatalf("подытог: %v", err)
		}
		code, body := doJSON(t, srv, http.MethodPost, path,
			map[string]any{"due_date": today.Format("2006-01-02")})
		if code != http.StatusConflict || body["error"] != "period_locked" {
			t.Fatalf("код %d, ошибка %v — ожидался 409/period_locked", code, body["error"])
		}
	})
}

// Менеджер видит, что выкладку завёл креатор, а не он: в списке и в
// карточке. Иначе он будет искать её в своих пачках и не найдёт.
func TestManagerSeesWhoAddedPublication(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	pubSvc := publications.NewService(publications.NewRepo(pool))
	if _, err := pubSvc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(354)},
		CreatedBy:      creators[1],
	}); err != nil {
		t.Fatalf("пачка менеджера: %v", err)
	}
	mine, err := pubSvc.AddSelfPublication(ctx, pid, creators[0], pubDay(356), time.Now().UTC())
	if err != nil {
		t.Fatalf("самодобавленная: %v", err)
	}

	items, err := pubSvc.ListForManager(ctx, pid)
	if err != nil {
		t.Fatalf("список менеджера: %v", err)
	}
	var seenSelf, seenManager bool
	for _, p := range items {
		switch {
		case p.ID == mine.ID:
			seenSelf = p.SelfAdded
		default:
			if p.SelfAdded {
				t.Errorf("выкладка менеджера помечена как самодобавленная: %s", p.ID)
			}
			seenManager = true
		}
	}
	if !seenSelf {
		t.Error("в списке менеджера не видно, что выкладку завёл креатор")
	}
	if !seenManager {
		t.Error("выкладки менеджера в списке нет — проверять не с чем")
	}

	card, err := pubSvc.Get(ctx, mine.ID)
	if err != nil {
		t.Fatalf("карточка: %v", err)
	}
	if !card.SelfAdded {
		t.Error("в карточке не видно, что выкладку завёл креатор")
	}
}
