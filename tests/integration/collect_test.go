package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/billing"
	"marketpclce/internal/instacurl"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// fakeCollector — вместо живого instacurl. Отдаёт то, что положили, и
// считает, сколько раз его дёрнули: «один ролик — не чаще раза в сутки»
// проверяется именно по числу вызовов, а не по числу строк в БД.
type fakeCollector struct {
	calls    int
	urlsSeen []string
	byURL    map[string]instacurl.Result
	err      error
}

func (f *fakeCollector) Collect(_ context.Context, urls []string) ([]instacurl.Result, error) {
	f.calls++
	f.urlsSeen = append(f.urlsSeen, urls...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]instacurl.Result, 0, len(urls))
	for _, u := range urls {
		if r, ok := f.byURL[u]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func okResult(url string, views, likes, comments int64) instacurl.Result {
	return instacurl.Result{
		Platform: "tiktok", URL: url, Kind: "media", OK: true,
		Posts: []instacurl.PostMetrics{{
			ID: "1", Views: &views, Likes: &likes, Comments: &comments,
		}},
	}
}

// setupSubmittedLinks — проект с одной выкладкой и сданными ссылками.
func setupSubmittedLinks(t *testing.T, urls ...string) (projectID uuid.UUID, creator uuid.UUID, cleanup func()) {
	t.Helper()
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
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
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          urls,
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	return projectID, creators[0], cleanup
}

// Жёсткое правило: один ролик обходится не чаще раза в сутки. Проверяем
// по числу походов в сервис — деньги тратятся там, а не в базе.
func TestCollectionOncePerDayHardLimit(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/777"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: okResult(url, 1000, 50, 7),
	}}
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo).WithCollector(fake)

	now := time.Now().UTC()
	st, err := svc.RunCollection(ctx, now, 50)
	if err != nil {
		t.Fatalf("RunCollection: %v", err)
	}
	if st.Considered != 1 || st.Saved != 1 {
		t.Fatalf("первый проход: %+v, ожидалось 1 рассмотрено и 1 сохранено", st)
	}

	// Второй проход в тот же день — сервис дёргаться не должен вообще.
	st, err = svc.RunCollection(ctx, now.Add(2*time.Hour), 50)
	if err != nil {
		t.Fatalf("RunCollection (повтор): %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("во второй раз за сутки взято %d ссылок, ожидалось 0", st.Considered)
	}
	if fake.calls != 1 {
		t.Errorf("в instacurl сходили %d раза, ожидался один — правило «раз в сутки» обойдено", fake.calls)
	}

	stats, err := repo.Stats(ctx, projectID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Views != 1000 || stats.Likes != 50 || stats.Comments != 7 {
		t.Errorf("итоги проекта %+v, ожидалось 1000/50/7", stats)
	}
	if stats.AsOf == nil {
		t.Error("нет даты последнего сбора — без неё цифру невозможно объяснить")
	}
}

// График 1→2→4→8 по возрасту ролика.
func TestCollectIntervalGrowsWithAge(t *testing.T) {
	cases := []struct{ age, want int }{
		{0, 1}, {5, 1},
		{6, 2}, {14, 2},
		{15, 4}, {28, 4},
		{29, 8}, {365, 8},
	}
	for _, c := range cases {
		if got := publications.CollectInterval(c.age); got != c.want {
			t.Errorf("возраст %d дней: интервал %d, ожидался %d", c.age, got, c.want)
		}
	}
}

// Снимок за ту же дату перезаписывается, а не плодит строки: повторный
// сбор ничего не портит (требование М4).
func TestCollectionSnapshotIsIdempotent(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/888"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	repo := publications.NewRepo(pool)
	now := time.Now().UTC()

	links, err := repo.DueForCollection(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueForCollection: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("ссылок к сбору %d, ожидалась одна", len(links))
	}

	v1, l1, c1 := int64(100), int64(1), int64(0)
	if err := repo.SaveStats(ctx, links[0], &v1, &l1, &c1, nil, now); err != nil {
		t.Fatalf("SaveStats: %v", err)
	}
	v2, l2, c2 := int64(250), int64(9), int64(3)
	if err := repo.SaveStats(ctx, links[0], &v2, &l2, &c2, nil, now); err != nil {
		t.Fatalf("SaveStats (повтор): %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM video_stat_daily WHERE link_id = $1`, links[0].LinkID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("строк снимка %d, ожидалась одна на дату", rows)
	}
	stats, _ := repo.Stats(ctx, projectID)
	if stats.Views != 250 {
		t.Errorf("просмотры %d, ожидалось 250 — повторный сбор должен перезаписывать", stats.Views)
	}
}

// Сервис ответил, но метрик не дал (площадка не поддержана, ролик удалён).
// Нулей в отчёте быть не должно: они врут сильнее, чем пропуск.
func TestCollectionNoDataDoesNotWriteZeros(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://vk.com/clip-1_2"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: {Platform: "vk", URL: url, Kind: "media", OK: false,
			Error: "VK media не поддержан"},
	}}
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo).WithCollector(fake)

	now := time.Now().UTC()
	st, err := svc.RunCollection(ctx, now, 50)
	if err != nil {
		t.Fatalf("RunCollection: %v", err)
	}
	if st.NoData != 1 || st.Saved != 0 {
		t.Fatalf("%+v, ожидалось no_data=1 saved=0", st)
	}

	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1`, projectID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("записано %d снимков с нулями — в отчёте появится ложный ноль", rows)
	}

	// Но следующий обход отодвинут: битая ссылка не должна дёргаться
	// на каждом тике.
	links, err := repo.DueForCollection(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("DueForCollection: %v", err)
	}
	if len(links) != 0 {
		t.Error("несобравшаяся ссылка снова в очереди через час")
	}
}

// Сервис лежит целиком — расписание не двигаем: наказывать ссылки за
// чужую недоступность нельзя, иначе после часа простоя все они уедут
// на сутки вперёд.
func TestCollectionServiceDownKeepsSchedule(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/999"
	_, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{err: errors.New("connection refused")}
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo).WithCollector(fake)

	now := time.Now().UTC()
	if _, err := svc.RunCollection(ctx, now, 50); err == nil {
		t.Fatal("падение сервиса должно возвращать ошибку, а не тихо проглатываться")
	}

	// Ссылка не выпала из очереди, но и не осталась первой: пачка
	// отодвинута на короткий backoff. Иначе следующий тик взял бы те же
	// ссылки, снова упёрся в таймаут — и сбор встал бы навсегда.
	if links, err := repo.DueForCollection(ctx, now, 10); err != nil {
		t.Fatalf("DueForCollection: %v", err)
	} else if len(links) != 0 {
		t.Error("после недоступности сервиса пачка не отодвинута — очередь заклинит")
	}
	if links, err := repo.DueForCollection(ctx, now.Add(20*time.Minute), 10); err != nil {
		t.Fatalf("DueForCollection (позже): %v", err)
	} else if len(links) != 1 {
		t.Error("ссылка не вернулась в очередь после backoff")
	}
}

// Без настроенной интеграции сбор обязан ругаться, а не молчать: пустой
// отчёт читается как «ролики никто не смотрит».
func TestCollectionRequiresCollector(t *testing.T) {
	pool := integration.Pool(t)
	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.RunCollection(context.Background(), time.Now(), 10); !errors.Is(err, publications.ErrCollectorNotSet) {
		t.Errorf("без клиента: got %v, want ErrCollectorNotSet", err)
	}
}

// Схлопывание: снимок записан ДО удаления ряда, и удаление необратимо.
// Если бы порядок был обратный, падение между шагами стёрло бы историю
// без замены.
func TestCollapseWritesSummaryBeforeDeleting(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/555"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	repo := publications.NewRepo(pool)
	now := time.Now().UTC()

	links, err := repo.DueForCollection(ctx, now, 10)
	if err != nil || len(links) != 1 {
		t.Fatalf("DueForCollection: %v, ссылок %d", err, len(links))
	}
	// Два дня истории: итог должен взять последний день, а не сумму —
	// просмотры накопительные.
	v1, l1, c1 := int64(400), int64(10), int64(2)
	if err := repo.SaveStats(ctx, links[0], &v1, &l1, &c1, nil, now.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("SaveStats day1: %v", err)
	}
	v2, l2, c2 := int64(900), int64(31), int64(5)
	if err := repo.SaveStats(ctx, links[0], &v2, &l2, &c2, nil, now); err != nil {
		t.Fatalf("SaveStats day2: %v", err)
	}

	// Проект закрыт 29 дней назад: срок сбора (28 дней) вышел.
	closedAt := now.AddDate(0, 0, -29)
	if err := repo.StopCollectionAfter(ctx, projectID, closedAt, publications.CollectionRetention); err != nil {
		t.Fatalf("StopCollectionAfter: %v", err)
	}

	// Месяц выкладки фиксируем: пока он идёт, схлопывать ежедневный ряд
	// нельзя — из него снимается срез месяца. Здесь это предусловие, а
	// не предмет проверки (см. TestCollapseKeepsLockedSnapshot).
	if _, err := billing.NewService(billing.NewRepo(pool)).
		LockMonth(ctx, projectID, pubDay(0), nil, now, now); err != nil {
		t.Fatalf("зафиксировать месяц: %v", err)
	}

	n, err := repo.CollapseFinished(ctx, now)
	if err != nil {
		t.Fatalf("CollapseFinished: %v", err)
	}
	if n != 1 {
		t.Fatalf("свёрнуто %d проектов, ожидался 1", n)
	}

	stats, err := repo.Stats(ctx, projectID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.Collapsed {
		t.Error("итоги должны читаться из снимка")
	}
	if stats.Views != 900 {
		t.Errorf("в снимке %d просмотров, ожидалось 900 — берётся последний день, а не сумма", stats.Views)
	}
	if stats.VideosCount != 1 {
		t.Errorf("роликов в снимке %d, ожидался 1", stats.VideosCount)
	}

	var daily int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM video_stat_daily WHERE link_id = $1`, links[0].LinkID).Scan(&daily); err != nil {
		t.Fatalf("count daily: %v", err)
	}
	if daily != 0 {
		t.Errorf("детальный ряд остался (%d строк) — схлопывание не доведено", daily)
	}

	// Ссылка снята с обхода: собирать больше нечего.
	due, err := repo.DueForCollection(ctx, now.AddDate(0, 0, 10), 10)
	if err != nil {
		t.Fatalf("DueForCollection: %v", err)
	}
	for _, l := range due {
		if l.LinkID == links[0].LinkID {
			t.Error("ссылка закрытого проекта снова в очереди сбора")
		}
	}
}

// Gauge'и: просроченные выкладки, застрявшие ссылки и отставание сбора.
// Без них «сбор выключен» и «сбор сломан» выглядят на дашборде одинаково.
func TestGaugesReflectRealState(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)
	now := time.Now().UTC()

	base, err := svc.RefreshGauges(ctx, now)
	if err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}

	// Две просроченные выкладки и одна сегодняшняя.
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-3), pubDay(-1), pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	g, err := svc.RefreshGauges(ctx, now)
	if err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}
	if got := g.PublicationsOverdue - base.PublicationsOverdue; got != 2 {
		t.Errorf("просроченных стало больше на %d, ожидалось 2 (сегодняшняя не считается)", got)
	}

	// Согласованный перенос снимает просрочку — и с gauge тоже.
	if _, err := svc.RequestDateChange(ctx, res.Items[0].ID, creators[0],
		pubDay(5), "перенос"); err != nil {
		t.Fatalf("RequestDateChange: %v", err)
	}
	g2, err := svc.RefreshGauges(ctx, now)
	if err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}
	if g2.PublicationsOverdue != g.PublicationsOverdue-1 {
		t.Errorf("после просьбы о переносе просроченных %d, ожидалось %d",
			g2.PublicationsOverdue, g.PublicationsOverdue-1)
	}

	// Ссылка, которую давно пора было обойти, попадает в stale.
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[2].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/1"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET next_collect_at = now() - interval '3 days'
WHERE publication_id = $1`, res.Items[2].ID); err != nil {
		t.Fatalf("age link: %v", err)
	}

	g3, err := svc.RefreshGauges(ctx, now)
	if err != nil {
		t.Fatalf("RefreshGauges: %v", err)
	}
	if g3.LinksStale <= base.LinksStale {
		t.Errorf("застрявших ссылок %d, ожидалось больше %d", g3.LinksStale, base.LinksStale)
	}
}

// Полнота, а не только успех: сервис может вернуть лайки и комментарии,
// но не просмотры — так выглядит отвалившийся разбор одного поля. Снимок
// при этом писать надо (лайки настоящие), но в отчёте по площадке будут
// нули, и метрика обязана это показать.
func TestCollectionRecordsPartialAsNoViews(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://likee.video/@u/video/42"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	likes, comments := int64(116430), int64(8180)
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: {
			Platform: "likee", URL: url, Kind: "media", OK: true,
			// Просмотров нет — ровно то, что случится, если разбор
			// описания перестанет их находить.
			Posts: []instacurl.PostMetrics{{ID: "42", Likes: &likes, Comments: &comments}},
		},
	}}
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo).WithCollector(fake)

	now := time.Now().UTC()
	st, err := svc.RunCollection(ctx, now, 50)
	if err != nil {
		t.Fatalf("RunCollection: %v", err)
	}
	if st.Saved != 1 {
		t.Fatalf("сохранено %d, ожидалось 1: лайки настоящие, их терять нельзя", st.Saved)
	}

	stats, err := repo.Stats(ctx, projectID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Likes != 116430 {
		t.Errorf("лайки %d, ожидалось 116430", stats.Likes)
	}
	if stats.Views != 0 {
		t.Errorf("просмотры %d, ожидался 0 — их не прислали", stats.Views)
	}

	// Снимок записан, но без просмотров: NULL, а не ноль. Разница важна —
	// ноль означает «никто не смотрел», NULL означает «не собрали».
	var viewsNull bool
	if err := pool.QueryRow(ctx, `
SELECT views IS NULL FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1`, projectID).Scan(&viewsNull); err != nil {
		t.Fatalf("check views null: %v", err)
	}
	if !viewsNull {
		t.Error("просмотры записаны нулём вместо NULL — в отчёте это станет «никто не смотрел»")
	}
}

// Двойной клик на массовом создании не должен плодить дубли: раньше
// повторная отправка давала вторую пачку на те же даты, CancelBatch снимал
// только одну, а по второй бесконечно шли напоминания.
func TestCreateBatchIsIdempotentOnRepeat(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	in := publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(1), pubDay(2), pubDay(3)},
		CreatedBy:      creators[0],
	}

	first, err := svc.CreateBatch(ctx, in)
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if first.Created != 3 {
		t.Fatalf("создано %d, ожидалось 3", first.Created)
	}

	// Тот же запрос ещё раз — как второй клик по кнопке.
	second, err := svc.CreateBatch(ctx, in)
	if err != nil {
		t.Fatalf("CreateBatch (повтор): %v", err)
	}
	if second.Created != 0 {
		t.Errorf("повтор создал %d выкладок, ожидалось 0", second.Created)
	}

	all, err := svc.ListForManager(ctx, projectID)
	if err != nil {
		t.Fatalf("ListForManager: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("в проекте %d выкладок, ожидалось 3 — повтор создал дубли", len(all))
	}

	// После отмены на ту же дату можно поставить заново: ограничение
	// выведено из-под отменённых.
	if _, err := svc.CancelBatch(ctx, projectID, first.BatchID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}
	again, err := svc.CreateBatch(ctx, in)
	if err != nil {
		t.Fatalf("CreateBatch (после отмены): %v", err)
	}
	if again.Created != 3 {
		t.Errorf("после отмены создано %d, ожидалось 3", again.Created)
	}
}

// Мелкая пачка не должна означать мелкую пропускную способность: за тик
// воркер прогоняет пачки подряд, пока есть что собирать. Здесь проверяем
// то же на уровне сервиса — очередь вычерпывается за несколько проходов
// и не зацикливается.
func TestCollectionDrainsQueueInSmallBatches(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	// Пять выкладок по пять площадок — 25 ссылок.
	dates := make([]time.Time, 0, 5)
	for i := 0; i < 5; i++ {
		dates = append(dates, pubDay(-i))
	}
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          dates,
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	results := map[string]instacurl.Result{}
	for i, p := range res.Items {
		urls := []string{
			fmt.Sprintf("https://www.tiktok.com/@u/video/%d", i),
			fmt.Sprintf("https://youtu.be/vid%08d", i),
			fmt.Sprintf("https://www.instagram.com/reel/RE%08d/", i),
			fmt.Sprintf("https://vk.com/clip-1_%d", i),
			fmt.Sprintf("https://likee.video/@u/video/%d", i),
		}
		if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
			PublicationID: p.ID, ActorUserID: creators[0], URLs: urls,
		}); err != nil {
			t.Fatalf("SubmitLinks: %v", err)
		}
		for _, u := range urls {
			canonical, perr := publications.ParseLink(u)
			if perr != nil {
				t.Fatalf("ParseLink(%q): %v", u, perr)
			}
			results[canonical.Canonical] = okResult(canonical.Canonical, 100, 10, 1)
		}
	}

	fake := &fakeCollector{byURL: results}
	svc = svc.WithCollector(fake)

	const batch = 10
	now := time.Now().UTC()
	var considered, saved, passes int
	for i := 0; i < 20; i++ {
		st, err := svc.RunCollection(ctx, now, batch)
		if err != nil {
			t.Fatalf("RunCollection #%d: %v", i+1, err)
		}
		if st.Considered == 0 {
			break
		}
		if st.Considered > batch {
			t.Fatalf("проход #%d взял %d ссылок, потолок %d", i+1, st.Considered, batch)
		}
		considered += st.Considered
		saved += st.Saved
		passes++
	}

	if considered != 25 {
		t.Errorf("собрано ссылок %d, ожидалось 25 — очередь не вычерпалась", considered)
	}
	if saved != 25 {
		t.Errorf("сохранено %d снимков, ожидалось 25", saved)
	}
	if passes != 3 {
		t.Errorf("проходов %d, ожидалось 3 (10+10+5)", passes)
	}
	if fake.calls != 3 {
		t.Errorf("походов в сервис %d, ожидалось 3", fake.calls)
	}
}

// Два воркера сосуществуют несколько секунд при каждом деплое. Одну и ту
// же ссылку они брать не должны: запись идемпотентна и данные бы не
// пострадали, но каждый лишний обход — это списанный кредит.
func TestDueForCollectionClaimsLinks(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/claim"
	_, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	repo := publications.NewRepo(pool)
	now := time.Now().UTC()

	first, err := repo.DueForCollection(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueForCollection: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("первый воркер взял %d ссылок, ожидалась 1", len(first))
	}

	// Второй воркер в ту же секунду — ссылка уже занята.
	second, err := repo.DueForCollection(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueForCollection (второй воркер): %v", err)
	}
	for _, l := range second {
		if l.LinkID == first[0].LinkID {
			t.Error("два воркера взяли одну ссылку — instacurl получит её дважды, кредит спишется дважды")
		}
	}

	// Аренда короткая: если воркер умер, ссылка вернётся в очередь сама.
	later, err := repo.DueForCollection(ctx, now.Add(15*time.Minute), 10)
	if err != nil {
		t.Fatalf("DueForCollection (после аренды): %v", err)
	}
	if len(later) != 1 {
		t.Error("после истечения аренды ссылка не вернулась в очередь")
	}
}

// Один и тот же ролик, сданный дважды, получает цифры по ОБЕИМ ссылкам.
//
// Так бывает, когда ролик сняли вдвоём и оба сдали одну ссылку, или
// когда креатор сдал один адрес в двух выкладках. В instacurl мы при
// этом ходим один раз — лишний поход стоит кредит, — но раскладывать
// результат обязаны на все ссылки с этим адресом. Иначе у одного
// креатора цифры есть, а у второго вечные нули, и в отчёте это читается
// как «его ролик никто не смотрел».
func TestCollectionFansOutToEveryLinkWithSameURL(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/424242"
	projectID, creator, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	// Вторая выкладка того же креатора с тем же адресом.
	svc0 := publications.NewService(publications.NewRepo(pool))
	res, err := svc0.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creator},
		Dates:          []time.Time{pubDay(1)},
		CreatedBy:      creator,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc0.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creator,
		URLs:          []string{url},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: okResult(url, 5000, 100, 10),
	}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)

	st, err := svc.RunCollection(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("RunCollection: %v", err)
	}
	if st.Considered != 2 {
		t.Fatalf("к сбору взято %d ссылок, ожидались обе", st.Considered)
	}
	if fake.calls != 1 {
		t.Errorf("в instacurl сходили %d раза — одинаковый адрес обязан дедуплицироваться", fake.calls)
	}
	if st.Saved != 2 {
		t.Errorf("сохранено снимков: %d, ожидались оба — иначе одна из ссылок останется без цифр", st.Saved)
	}

	// И в базе снимок есть у каждой ссылки, а не у последней.
	var withStats int
	if err := pool.QueryRow(ctx, `
SELECT count(DISTINCT l.id)
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
JOIN video_stat_daily d ON d.link_id = l.id
WHERE p.project_id = $1`, projectID).Scan(&withStats); err != nil {
		t.Fatalf("подсчёт снимков: %v", err)
	}
	if withStats != 2 {
		t.Errorf("цифры получили %d ссылки из 2 — у второй в отчёте будут нули", withStats)
	}
}

// okResultPublished — тот же ответ, но с датой публикации.
func okResultPublished(url, publishedAt string, views int64) instacurl.Result {
	r := okResult(url, views, 0, 0)
	r.Posts[0].PublishedAt = publishedAt
	return r
}

// Дата публикации сохраняется при сборе, пишется один раз и переживает
// её отсутствие в ответе.
//
// До этого PublishedAt приходил на каждом обходе и выбрасывался: в базе
// его не было вовсе, и «сколько ролику дней» ответить было нечем.
func TestPublishedAtSavedOnceAndSurvivesAbsence(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/pub42"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	repo := publications.NewRepo(pool)
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: okResultPublished(url, "2026-09-02T10:30:00Z", 1000),
	}}
	svc := publications.NewService(repo).WithCollector(fake)

	// Собираем «сейчас»: ссылки заводятся с next_collect_at = now(), и
	// проход задним числом просто ничего бы не взял.
	day := time.Now().UTC()
	if _, err := svc.RunCollection(ctx, day, 50); err != nil {
		t.Fatalf("первый сбор: %v", err)
	}
	got := linkPublishedAt(t, pool, projectID)
	if got == nil {
		t.Fatal("дата публикации не сохранилась")
	}
	want := time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("дата публикации %v, ожидали %v", got, want)
	}

	// Источник передумал: прислал другую дату. Не переписываем — ролик
	// не выходит дважды, а источник со временем начинает врать.
	fake.byURL[url] = okResultPublished(url, "2026-09-05T00:00:00Z", 2000)
	if _, err := svc.RunCollection(ctx, day.AddDate(0, 0, 2), 50); err != nil {
		t.Fatalf("второй сбор: %v", err)
	}
	if got := linkPublishedAt(t, pool, projectID); got == nil || !got.Equal(want) {
		t.Errorf("дата публикации переписана: %v, ожидали %v", got, want)
	}

	// Источник перестал отдавать дату — сохранённое остаётся на месте, а
	// сбор метрик не ломается.
	fake.byURL[url] = okResult(url, 3000, 0, 0)
	st, err := svc.RunCollection(ctx, day.AddDate(0, 0, 6), 50)
	if err != nil {
		t.Fatalf("третий сбор: %v", err)
	}
	if st.Saved != 1 {
		t.Errorf("сбор без даты публикации не сохранился: %+v", st)
	}
	if got := linkPublishedAt(t, pool, projectID); got == nil || !got.Equal(want) {
		t.Errorf("дата публикации пропала при сборе без неё: %v", got)
	}

	// Производное «когда ролик вышел» на выкладке — самое раннее среди
	// площадок; у одной площадки это она же.
	pubs, err := repo.ListByProject(ctx, projectID)
	if err != nil {
		t.Fatalf("список выкладок: %v", err)
	}
	if len(pubs) == 0 || pubs[0].PublishedAt == nil {
		t.Fatalf("у выкладки нет даты выхода: %+v", pubs)
	}
	if !pubs[0].PublishedAt.Equal(want) {
		t.Errorf("дата выхода выкладки %v, ожидали %v", pubs[0].PublishedAt, want)
	}
}

// Мусор в поле даты не пишется: «не знаем» честнее выдуманной даты.
func TestPublishedAtIgnoresGarbage(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/pub43"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: okResultPublished(url, "позавчера", 500),
	}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)

	st, err := svc.RunCollection(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("сбор: %v", err)
	}
	if st.Saved != 1 {
		t.Fatalf("метрики не сохранились из-за мусора в дате: %+v", st)
	}
	if got := linkPublishedAt(t, pool, projectID); got != nil {
		t.Errorf("в базу уехала дата из мусора: %v", got)
	}
}

func linkPublishedAt(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) *time.Time {
	t.Helper()
	var out *time.Time
	if err := pool.QueryRow(context.Background(), `
SELECT l.published_at
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1
ORDER BY l.submitted_at
LIMIT 1`, projectID).Scan(&out); err != nil {
		t.Fatalf("дата публикации ссылки: %v", err)
	}
	return out
}
