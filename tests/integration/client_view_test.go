package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/instacurl"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// В ленту клиента попадает только то, что можно посмотреть: выкладка со
// сданными ссылками. Дата в плане — ещё не ролик.
func TestClientFeedShowsOnlyPublished(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-1), pubDay(1)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs: []string{
			"https://www.tiktok.com/@u/video/1",
			"https://youtu.be/dQw4w9WgXcQ",
		},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	feed, err := svc.ClientFeed(ctx, projectID, 50)
	if err != nil {
		t.Fatalf("ClientFeed: %v", err)
	}
	if len(feed) != 1 {
		t.Fatalf("в ленте %d роликов, ожидался 1 (второй ещё не сдан)", len(feed))
	}
	if len(feed[0].Platforms) != 2 {
		t.Errorf("площадок %d, ожидалось 2", len(feed[0].Platforms))
	}
	if feed[0].CreatorUserID != creators[0] {
		t.Error("в ленте не тот креатор")
	}
}

// Клиент не видит просрочек. Выкладка с прошедшей датой и без ссылок
// остаётся для него «запланированной» — разбирается с отставанием
// менеджер, а не заказчик с креатором напрямую.
// Календарь ставит ролик в день, когда он ВЫШЕЛ, а не когда планировался.
//
// Плановая дата и фактическая расходятся постоянно: выкладывают раньше
// или позже, ссылки сдают через день. Раньше календарь всегда
// группировал по плановой, и вышедший ролик стоял не в своём дне —
// причём на той же странице лента роликов показывала настоящую дату.
// Два ответа на один вопрос рядом друг с другом.
//
// Это не косметика: от факта выхода отсчитываются периоды, ступени и
// деньги. Календарь заказчика обязан говорить о том же дне, что и всё
// остальное.
func TestClientCalendarUsesPublishedDate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	planned := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	actual := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{planned},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://vk.com/clip-1_777"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	// Дату выхода ставит сборщик — здесь кладём её сами: воспроизводить
	// обход пяти площадок ради одного поля незачем.
	if _, err := pool.Exec(ctx,
		`UPDATE publication_links SET published_at = $2 WHERE publication_id = $1`,
		res.Items[0].ID, actual); err != nil {
		t.Fatalf("дата выхода: %v", err)
	}

	days, err := svc.Calendar(ctx, projectID, month)
	if err != nil {
		t.Fatalf("Calendar: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("дней в календаре %d, ожидался 1", len(days))
	}
	if got := days[0].Date.Day(); got != 8 {
		t.Errorf("ролик стоит на %d-м, а вышел 8-го (планировался на 10-е) — "+
			"календарь спорит с лентой роликов на той же странице", got)
	}
}

func TestClientCalendarHidesOverdue(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates: []time.Time{
			time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		},
		CreatedBy: creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	// Первую сдали, вторая просрочена (дата в прошлом, ссылок нет).
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://vk.com/clip-1_2"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	days, err := svc.Calendar(ctx, projectID, month)
	if err != nil {
		t.Fatalf("Calendar: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("дней в календаре %d, ожидалось 2", len(days))
	}

	var published, planned int
	for _, d := range days {
		published += d.Published
		planned += d.Planned
		for _, it := range d.Items {
			if it.Status != publications.ClientStatusPublished &&
				it.Status != publications.ClientStatusPlanned {
				t.Errorf("клиенту утёк внутренний статус: %s", it.Status)
			}
		}
	}
	if published != 1 || planned != 1 {
		t.Errorf("вышло %d, запланировано %d; ожидалось 1 и 1", published, planned)
	}
}

// Отменённые выкладки клиенту не показываются вовсе.
func TestClientCalendarSkipsCancelled(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	month := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if n, err := svc.CancelBatch(ctx, projectID, res.BatchID); err != nil || n != 1 {
		t.Fatalf("CancelBatch: n=%d err=%v", n, err)
	}

	days, err := svc.Calendar(ctx, projectID, month)
	if err != nil {
		t.Fatalf("Calendar: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("отменённая выкладка попала в календарь клиента: %+v", days)
	}
}

// Настройки уведомлений: по умолчанию включено всё, кроме порога.
// Частичное обновление не сбрасывает соседние переключатели.
func TestClientPrefsDefaultsAndPartialUpdate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, _, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	var clientID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, projectID).Scan(&clientID); err != nil {
		t.Fatalf("read client: %v", err)
	}

	svc := publications.NewService(publications.NewRepo(pool))
	prefs, err := svc.Prefs(ctx, projectID, clientID)
	if err != nil {
		t.Fatalf("Prefs: %v", err)
	}
	if !prefs.OnNewVideo || !prefs.OnWeeklyDigest || !prefs.OnDateShift {
		t.Error("по умолчанию уведомления должны быть включены")
	}
	if prefs.ViewsThreshold != nil {
		t.Error("порог просмотров по умолчанию не задан")
	}

	threshold := int64(100000)
	prefs.ViewsThreshold = &threshold
	prefs.OnWeeklyDigest = false
	if err := svc.SavePrefs(ctx, prefs); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}

	got, err := svc.Prefs(ctx, projectID, clientID)
	if err != nil {
		t.Fatalf("Prefs: %v", err)
	}
	if got.ViewsThreshold == nil || *got.ViewsThreshold != 100000 {
		t.Errorf("порог %v, ожидалось 100000", got.ViewsThreshold)
	}
	if got.OnWeeklyDigest {
		t.Error("недельная сводка должна была выключиться")
	}
	if !got.OnNewVideo {
		t.Error("соседний переключатель сбросился — частичное обновление сломано")
	}

	// Ноль означает «не уведомлять», а не «порог в ноль просмотров».
	zero := int64(0)
	got.ViewsThreshold = &zero
	if err := svc.SavePrefs(ctx, got); err != nil {
		t.Fatalf("SavePrefs(0): %v", err)
	}
	after, _ := svc.Prefs(ctx, projectID, clientID)
	if after.ViewsThreshold != nil {
		t.Errorf("нулевой порог сохранился как %v — уведомления пошли бы на каждый ролик", after.ViewsThreshold)
	}
}

// «Ролик перешагнул порог» приходит ОДИН раз, а не каждый день, пока
// просмотры остаются выше.
func TestThresholdNotifiedOnlyOnce(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/threshold"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	var clientID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, projectID).Scan(&clientID); err != nil {
		t.Fatalf("read client: %v", err)
	}

	repo := publications.NewRepo(pool)
	threshold := int64(1000)
	if err := repo.SavePrefs(ctx, publications.NotificationPrefs{
		ProjectID: projectID, UserID: clientID,
		OnNewVideo: true, OnWeeklyDigest: true, OnDateShift: true,
		ViewsThreshold: &threshold,
	}); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: okResult(url, 5000, 100, 10),
	}}
	svc := publications.NewService(repo).WithCollector(fake)

	now := time.Now().UTC()
	st, err := svc.RunCollection(ctx, now, 50)
	if err != nil {
		t.Fatalf("RunCollection: %v", err)
	}
	if st.ThresholdNotified != 1 {
		t.Fatalf("уведомлений %d, ожидалось 1", st.ThresholdNotified)
	}

	// Следующий день: просмотры всё ещё выше порога, но писать второй раз
	// нельзя — человек уже знает.
	fake.byURL[url] = okResult(url, 9000, 200, 20)
	tomorrow := now.AddDate(0, 0, 1)
	st, err = svc.RunCollection(ctx, tomorrow, 50)
	if err != nil {
		t.Fatalf("RunCollection (завтра): %v", err)
	}
	if st.ThresholdNotified != 0 {
		t.Errorf("уведомление ушло второй раз (%d) — клиент получит его каждый день", st.ThresholdNotified)
	}
}

// «Вышел» в ленте — это дата ВЫХОДА, а не момент, когда креатор прислал
// ссылку. События разные: ролик мог выйти 31 июля, а ссылка приехать
// через неделю. Подписывать весь месяц работы одним днём нельзя — на
// стенде именно так и выглядело.
func TestClientFeedShowsPublishDateNotSubmitDate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))

	// Оба ролика СДАНЫ сегодня, а вышли в разные дни. Лента обязана
	// показать выход, и порядок — по нему же.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	older := today.AddDate(0, 0, -30)
	newer := today.AddDate(0, 0, -5)

	first := publishOn(t, pool, projectID, creators[0], today, "fdt01", 100)
	second := publishOn(t, pool, projectID, creators[0], today, "fdt02", 100)
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET published_at = $2, submitted_at = $3
WHERE publication_id = $1`, first, older, today); err != nil {
		t.Fatalf("даты первого: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET published_at = $2, submitted_at = $3
WHERE publication_id = $1`, second, newer, today); err != nil {
		t.Fatalf("даты второго: %v", err)
	}
	// Третий: площадка даты выхода не отдала — остаётся момент сдачи.
	noDate := publishOn(t, pool, projectID, creators[0], today, "fdt03", 100)
	submitDay := today.AddDate(0, 0, -10)
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET published_at = NULL, submitted_at = $2
WHERE publication_id = $1`, noDate, submitDay); err != nil {
		t.Fatalf("даты третьего: %v", err)
	}

	feed, err := svc.ClientFeed(ctx, projectID, 50)
	if err != nil {
		t.Fatalf("ClientFeed: %v", err)
	}
	if len(feed) != 3 {
		t.Fatalf("в ленте %d роликов, ожидалось 3", len(feed))
	}

	got := make(map[uuid.UUID]time.Time, 3)
	for _, v := range feed {
		got[v.PublicationID] = v.PublishedAt.UTC().Truncate(24 * time.Hour)
	}
	if !got[first].Equal(older) {
		t.Errorf("первый ролик подписан %s, а вышел %s",
			got[first].Format("2006-01-02"), older.Format("2006-01-02"))
	}
	if !got[second].Equal(newer) {
		t.Errorf("второй ролик подписан %s, а вышел %s",
			got[second].Format("2006-01-02"), newer.Format("2006-01-02"))
	}
	if !got[noDate].Equal(submitDay) {
		t.Errorf("ролик без даты выхода подписан %s, ожидалась дата сдачи %s",
			got[noDate].Format("2006-01-02"), submitDay.Format("2006-01-02"))
	}

	// Сортировка — по той же величине, что и показ: новые сверху.
	wantOrder := []uuid.UUID{second, noDate, first}
	for i, want := range wantOrder {
		if feed[i].PublicationID != want {
			t.Fatalf("порядок ленты поехал относительно подписей: на месте %d стоит %s",
				i, feed[i].PublishedAt.Format("2006-01-02"))
		}
	}
}

// Календарь показывает ОДИН месяц, и открывался он всегда на текущем.
// Период проекта катится от даты первой публикации и на календарный
// месяц не ложится: выкладки регулярно оказывались в соседнем месяце, и
// заказчик видел пустую сетку — то есть «календарь не показывает
// выкладки». Чтобы экран мог открыть нужный месяц и назвать остальные,
// сервер говорит, в каких месяцах у проекта вообще что-то есть.
func TestCalendarMonthsListsEveryMonthWithPublications(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))

	// Три месяца подряд, начиная с позапрошлого: ровно та картина, на
	// которой ломался календарь.
	first := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	third := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{first, second, third},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	months, err := svc.CalendarMonths(ctx, projectID)
	if err != nil {
		t.Fatalf("CalendarMonths: %v", err)
	}
	want := []string{"2026-07", "2026-08", "2026-09"}
	if len(months) != len(want) {
		t.Fatalf("месяцев %d (%v), ожидалось %d", len(months), months, len(want))
	}
	for i, m := range want {
		if months[i] != m {
			t.Fatalf("месяц %d = %q, ожидался %q (список обязан идти по возрастанию)", i, months[i], m)
		}
	}

	// Вышедший ролик числится в месяце, в котором ВЫШЕЛ, а не в том, на
	// который его планировали. Список месяцев обязан считать день тем же
	// выражением, что и сама сетка: иначе полоса месяцев позовёт в
	// октябрь, а точка будет стоять в сентябре — пустой месяц по клику
	// из подсказки, которая ради этого и сделана.
	if _, err := pool.Exec(ctx, `
INSERT INTO publication_links (publication_id, platform, url, url_canonical, published_at)
VALUES ($1, 'tiktok', 'https://tiktok.com/x', 'https://tiktok.com/x', $2)`,
		res.Items[2].ID, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	months, err = svc.CalendarMonths(ctx, projectID)
	if err != nil {
		t.Fatalf("CalendarMonths после выхода: %v", err)
	}
	for _, m := range months {
		if m == "2026-09" {
			t.Fatalf("месяц взят по плановой дате, а ролик вышел в августе: %v", months)
		}
	}

	// Отменённая выкладка не видна и в самой сетке — значит, месяц, где
	// осталась только она, не должен звать в себя из полосы месяцев.
	if _, err := pool.Exec(ctx,
		`UPDATE project_publications SET status = 'cancelled' WHERE id = $1`,
		res.Items[0].ID); err != nil {
		t.Fatalf("cancel publication: %v", err)
	}
	months, err = svc.CalendarMonths(ctx, projectID)
	if err != nil {
		t.Fatalf("CalendarMonths после отмены: %v", err)
	}
	for _, m := range months {
		if m == "2026-07" {
			t.Fatalf("месяц с одной отменённой выкладкой остался в списке: %v", months)
		}
	}
	// Остался август: и вторая выкладка по плану, и третья по факту выхода.
	if len(months) != 1 || months[0] != "2026-08" {
		t.Fatalf("месяцы %v, ожидался только 2026-08", months)
	}
}
