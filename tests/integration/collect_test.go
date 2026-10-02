package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// Частоту задаёт расписание, и только оно.
//
// Раньше здесь проверялось жёсткое правило «один ролик не чаще раза в
// календарный день». Правило снято намеренно: пока оно стояло, шаг
// меньше суток был невыразим, и у ролика, вышедшего час назад, в
// кабинете честно висел ноль до следующей полуночи.
//
// Что проверяется вместо него, тремя шагами подряд:
//
//  1. фоновый обход соблюдает свой шаг — раньше срока в сервис не
//     ходим, деньги тратятся там, а не в базе;
//  2. открытие карточки обновляет цифру, хотя фоновый срок ещё не
//     настал, — ради этого вся затея;
//  3. снимок за сутки при этом ПЕРЕЗАПИСЫВАЕТСЯ, а не плодит строки.
//     На третьем держится весь отчёт: итог считается по последнему
//     снимку каждой ссылки, и две строки за один день сложились бы в
//     двойные просмотры.
func TestCollectionFollowsSchedule(t *testing.T) {
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

	// Фоновый шаг свежего ролика — сутки. Через два часа в сервис идти
	// не за чем: на эти цифры сейчас никто не смотрит.
	st, err = svc.RunCollection(ctx, now.Add(2*time.Hour), 50)
	if err != nil {
		t.Fatalf("RunCollection (раньше срока): %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("раньше срока взято %d ссылок, ожидалось 0", st.Considered)
	}
	if fake.calls != 1 {
		t.Errorf("в instacurl сходили %d раза, ожидался один — фоновый шаг обойдён", fake.calls)
	}

	// А вот открытие карточки обновляет, хотя фоновый срок не настал:
	// ролику двенадцать минут, шаг для такого возраста — десять. И цифра
	// ложится ПОВЕРХ сегодняшнего снимка, а не рядом с ним.
	//
	// Двенадцать, а не ровно шесть: на границе шага тест зависел бы от
	// миллисекунд между посевом ссылки и взятием now — возраст уезжал
	// на следующую ступень, и «пора» превращалось в «ещё рано».
	fake.byURL[url] = okResult(url, 1500, 60, 9)
	st, err = svc.RefreshProject(ctx, projectID, nil, now.Add(12*time.Minute), 25)
	if err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	if st.Saved != 1 {
		t.Fatalf("по заходу в карточку: %+v, ожидалось 1 сохранено", st)
	}
	var rowsToday int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1 AND d.stat_date = $2::date`, projectID, now).Scan(&rowsToday); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowsToday != 1 {
		t.Fatalf("за сутки %d снимков, ожидался один — повторный обход плодит строки, и итог удвоится",
			rowsToday)
	}

	stats, err := repo.Stats(ctx, projectID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Views != 1500 || stats.Likes != 60 || stats.Comments != 9 {
		t.Errorf("итоги проекта %+v, ожидалось 1500/60/9 — в снимке должна остаться свежая цифра", stats)
	}
	if stats.AsOf == nil {
		t.Error("нет даты последнего сбора — без неё цифру невозможно объяснить")
	}
}

// Фоновое расписание — про РАСХОД.
//
// Архив почти не меняется, а обход стоит кредит у поставщика, поэтому
// 1→2→4→8 дней по возрасту ролика. Шаг, уменьшающийся с возрастом, — это
// тихий перерасход кредитов на архиве, и монотонность проверяется
// отдельно от значений.
//
// Второго расписания здесь больше нет. Обновление по заходу в карточку
// затухающего шага не имеет вовсе: просмотры подтягиваются КАЖДЫЙ раз,
// когда заходят в кабинет (решение владельца от 2 октября), а от
// двойного счёта одного захода защищает refreshDebounce — полминуты, и
// это не расписание.
func TestCollectSchedules(t *testing.T) {
	min, hour, day := time.Minute, time.Hour, 24*time.Hour

	background := []struct {
		age  time.Duration
		want time.Duration
	}{
		{0, day}, {min, day}, {6 * hour, day}, {5 * day, day},
		{6 * day, 2 * day}, {14 * day, 2 * day},
		{15 * day, 4 * day}, {28 * day, 4 * day},
		{29 * day, 8 * day}, {365 * day, 8 * day},
	}
	for _, c := range background {
		if got := publications.CollectEvery(c.age); got != c.want {
			t.Errorf("фоновый шаг для возраста %s: %s, ожидался %s", c.age, got, c.want)
		}
	}

	var prev time.Duration
	for _, c := range background {
		got := publications.CollectEvery(c.age)
		if got < prev {
			t.Fatalf("фоновое расписание не монотонно на возрасте %s", c.age)
		}
		prev = got
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
	if err := repo.SaveStats(ctx, links[0], &v1, &l1, &c1, nil, nil, now); err != nil {
		t.Fatalf("SaveStats: %v", err)
	}
	v2, l2, c2 := int64(250), int64(9), int64(3)
	if err := repo.SaveStats(ctx, links[0], &v2, &l2, &c2, nil, nil, now); err != nil {
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

	// Но следующий обход отодвинут на шаг расписания: битая ссылка не
	// должна дёргаться на каждом тике.
	//
	// Шаг для только что сданного ролика — минута, и это не оплошность:
	// свежий ролик площадка отдаёт не сразу, и «не нашли через минуту
	// после выкладки» чаще значит «ещё не проиндексировали», чем «не
	// поддержано». Поэтому ранний повтор здесь полезнее экономии.
	links, err := repo.DueForCollection(ctx, now.Add(30*time.Second), 10)
	if err != nil {
		t.Fatalf("DueForCollection: %v", err)
	}
	if len(links) != 0 {
		t.Error("несобравшаяся ссылка вернулась в очередь раньше срока")
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
	if err := repo.SaveStats(ctx, links[0], &v1, &l1, &c1, nil, nil, now.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("SaveStats day1: %v", err)
	}
	v2, l2, c2 := int64(900), int64(31), int64(5)
	if err := repo.SaveStats(ctx, links[0], &v2, &l2, &c2, nil, nil, now); err != nil {
		t.Fatalf("SaveStats day2: %v", err)
	}

	// Проект закрыт 29 дней назад: срок сбора (28 дней) вышел.
	closedAt := now.AddDate(0, 0, -29)
	if err := repo.StopCollectionAfter(ctx, projectID, closedAt, publications.CollectionRetention); err != nil {
		t.Fatalf("StopCollectionAfter: %v", err)
	}

	// Периоды выкладки подытоживаем: пока период идёт, схлопывать
	// ежедневный ряд нельзя — из него снимается срез. Здесь это
	// предусловие, а не предмет проверки (см.
	// TestCollapseKeepsLockedPeriodSnapshot).
	if _, _, err := billing.NewService(billing.NewRepo(pool)).
		LockDuePeriods(ctx, now.AddDate(0, 2, 0), billing.DefaultPeriodLockDelay); err != nil {
		t.Fatalf("подытожить периоды: %v", err)
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

// Репосты сохраняются при сборе, участвуют в вовлечённости, а их
// отсутствие поднимает звёздочку.
//
// ER = (лайки + комментарии + репосты) ÷ просмотры. Площадки отдают
// репосты не все, и показатель, посчитанный без них, занижен — молчать
// об этом нельзя.
func TestSharesFeedEngagementAndRaiseFlag(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const tiktok = "https://www.tiktok.com/@u/video/er01"
	const youtube = "https://www.youtube.com/shorts/er01"
	projectID, _, cleanup := setupSubmittedLinks(t, tiktok, youtube)
	defer cleanup()

	// TikTok отдал репосты, YouTube — нет.
	withShares := okResult(tiktok, 10_000, 500, 100)
	shares := int64(400)
	withShares.Posts[0].Shares = &shares
	noShares := okResult(youtube, 10_000, 500, 100)
	noShares.Platform = "youtube"

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		tiktok: withShares, youtube: noShares,
	}}
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo).WithCollector(fake)
	if _, err := svc.RunCollection(ctx, time.Now().UTC(), 50); err != nil {
		t.Fatalf("сбор: %v", err)
	}

	rep, err := svc.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("отчёт: %v", err)
	}

	byPlatform := map[string]publications.PlatformRow{}
	for _, p := range rep.ByPlatform {
		byPlatform[p.Platform] = p
	}
	tt := byPlatform["tiktok"]
	if tt.Shares == nil || *tt.Shares != 400 {
		t.Fatalf("репосты tiktok: %v", tt.Shares)
	}
	if tt.ERWithoutShares {
		t.Error("у площадки с репостами звёздочки быть не должно")
	}
	// (500 + 100 + 400) / 10000 = 10%
	if tt.ERPercent == nil || *tt.ERPercent < 9.99 || *tt.ERPercent > 10.01 {
		t.Errorf("ER tiktok %v, ожидали 10%%", tt.ERPercent)
	}

	yt := byPlatform["youtube"]
	if yt.Shares != nil {
		t.Errorf("у площадки без репостов сумма репостов %v — ноль означал бы «репостов нет»", yt.Shares)
	}
	if !yt.ERWithoutShares {
		t.Error("площадка без репостов не подняла звёздочку")
	}
	// (500 + 100) / 10000 = 6%: считаем без репостов, но признаёмся.
	if yt.ERPercent == nil || *yt.ERPercent < 5.99 || *yt.ERPercent > 6.01 {
		t.Errorf("ER youtube %v, ожидали 6%%", yt.ERPercent)
	}

	// По проекту звёздочка поднята: одна неизвестная площадка делает
	// приблизительным весь итог.
	if !rep.ERWithoutShares {
		t.Error("в отчёте проекта нет звёздочки, хотя одна площадка репостов не отдала")
	}
	if rep.Shares != nil {
		t.Errorf("сумма репостов по проекту %v, ожидали пусто", rep.Shares)
	}
}

// Ноль репостов и отсутствие репостов — разные вещи.
func TestZeroSharesDifferFromUnknown(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/er02"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	zero := int64(0)
	res := okResult(url, 1000, 10, 5)
	res.Posts[0].Shares = &zero
	fake := &fakeCollector{byURL: map[string]instacurl.Result{url: res}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)
	if _, err := svc.RunCollection(ctx, time.Now().UTC(), 50); err != nil {
		t.Fatalf("сбор: %v", err)
	}

	rep, err := svc.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("отчёт: %v", err)
	}
	if rep.ERWithoutShares {
		t.Error("ноль репостов — это ответ, а не отсутствие: звёздочки быть не должно")
	}
	if rep.Shares == nil || *rep.Shares != 0 {
		t.Errorf("репосты %v, ожидали ноль", rep.Shares)
	}
	// (10 + 5 + 0) / 1000 = 1.5%
	if rep.ERPercent == nil || *rep.ERPercent < 1.49 || *rep.ERPercent > 1.51 {
		t.Errorf("ER %v, ожидали 1.5%%", rep.ERPercent)
	}
}

// Каждый заход в кабинет подтягивает просмотры заново — но один заход
// платится один раз.
//
// Решение владельца от 2 октября: цифры тянем каждый раз, когда заходят.
// Затухающего расписания свежести (минута, пять, десять, полчаса…)
// больше нет — пока человек смотрит на экран, он смотрит на сегодняшнее
// число.
//
// Остался только дебаунс в полминуты, и он не про свежесть: на карточке
// обновление просят два виджета, F5 повторяет запрос, вторая вкладка
// открывает тот же проект — это ОДНО «зашли», и платить за него дважды
// незачем. Через полминуты это уже второй заход, и он честно идёт в
// сборщик, даже если ролику минута от рождения.
func TestRefreshPullsOnEveryVisit(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/555"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: okResult(url, 100, 1, 1),
	}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)

	now := time.Now().UTC()
	st, err := svc.RefreshProject(ctx, projectID, nil, now, 25)
	if err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	if st.Saved != 1 {
		t.Fatalf("первое открытие: %+v, ожидалось 1 сохранено", st)
	}

	// Тот же заход (второй виджет, F5, вторая вкладка) — второй раз не
	// платим.
	st, err = svc.RefreshProject(ctx, projectID, nil, now.Add(5*time.Second), 25)
	if err != nil {
		t.Fatalf("RefreshProject (тот же заход): %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("повтор одного захода взял %d ссылок, ожидалось 0", st.Considered)
	}
	if fake.calls != 1 {
		t.Errorf("в instacurl сходили %d раза, ожидался один — дебаунс обойдён", fake.calls)
	}

	// А это уже второй заход — и цифры тянутся снова, хотя ролику
	// меньше минуты. По прежнему расписанию здесь был бы отказ: шаг для
	// такого возраста равнялся минуте.
	st, err = svc.RefreshProject(ctx, projectID, nil, now.Add(45*time.Second), 25)
	if err != nil {
		t.Fatalf("RefreshProject (второй заход): %v", err)
	}
	if st.Saved != 1 {
		t.Errorf("второй заход: %+v, ожидалось 1 сохранено", st)
	}
	if fake.calls != 2 {
		t.Errorf("в instacurl сходили %d раза, ожидалось два", fake.calls)
	}
}

// Сбор не настроен — ручка обновления говорит об этом, а не делает вид.
//
// Молчаливый успех здесь хуже отказа: «сбор выключен» и «просмотров
// нет» выглядят на экране одинаково, и ровно на этом 1 октября 2026
// потерялся целый день — в окружении прода не было INSTACURL_URL, а
// кабинет показывал честные нули.
func TestRefreshProjectWithoutCollectorSaysSo(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, _, cleanup := setupSubmittedLinks(t, "https://www.tiktok.com/@u/video/556")
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	_, err := svc.RefreshProject(ctx, projectID, nil, time.Now().UTC(), 25)
	if !errors.Is(err, publications.ErrCollectorNotSet) {
		t.Errorf("%v, ожидалось ErrCollectorNotSet", err)
	}
}

// fakeExpander — разворачиватель коротких ссылок для теста. Ходить в
// живой TikTok из прогона нельзя: тест стал бы зависеть от сети и от
// того, жив ли сегодня чужой редирект.
type fakeExpander struct {
	to   string
	err  error
	last string
}

func (f *fakeExpander) Expand(_ context.Context, raw string) (string, error) {
	f.last = raw
	return f.to, f.err
}

// Короткая ссылка «поделиться» разворачивается при сдаче.
//
// В vt.tiktok.com/ZSbyuhrDf нет ни автора, ни id ролика — только код
// редиректа. Сборщик такую ссылку принимает за АККАУНТ (его ответ:
// «kind: profile, handle: ZSbyuhrDf, Account doesn't exist»), а та же
// ссылка в полном виде отдаёт просмотры. Пока мы сохраняли короткую,
// у вышедшего ролика в кабинете стоял ноль — неотличимый от «никто не
// смотрит».
func TestSubmitExpandsShortTikTokLink(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	const full = "https://www.tiktok.com/@ad.dobrotsen/video/7691706681415470356"
	exp := &fakeExpander{to: full}
	svc := publications.NewService(publications.NewRepo(pool)).WithURLExpander(exp)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	pub, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://vt.tiktok.com/ZSbyuhrDf"},
	})
	if err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}
	if exp.last != "https://vt.tiktok.com/ZSbyuhrDf" {
		t.Errorf("разворачивали %q, ожидали короткую ссылку", exp.last)
	}
	if len(pub.Links) != 1 {
		t.Fatalf("ссылок %d, ожидалась одна", len(pub.Links))
	}

	// В базу уходит ПОЛНЫЙ адрес: по нему сборщик и находит ролик.
	var canonical, mediaID string
	if err := pool.QueryRow(ctx, `
SELECT url_canonical, COALESCE(external_media_id, '')
FROM publication_links WHERE publication_id = $1`, res.Items[0].ID).Scan(&canonical, &mediaID); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if canonical != full {
		t.Errorf("сохранили %q, ожидали %q", canonical, full)
	}
	if mediaID != "7691706681415470356" {
		t.Errorf("id ролика %q — без него ссылку не опознать", mediaID)
	}
}

// Редирект не ответил — сдача всё равно проходит.
//
// Ролик уже вышел, человек его сдаёт, и ронять сдачу из-за недоступного
// редиректа значит потерять работу ради аккуратности адреса. Хуже, чем
// было, при этом не становится: раньше короткая ссылка сохранялась
// всегда.
func TestSubmitKeepsShortLinkWhenExpandFails(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	exp := &fakeExpander{err: errors.New("dial tcp: i/o timeout")}
	svc := publications.NewService(publications.NewRepo(pool)).WithURLExpander(exp)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://vt.tiktok.com/ZSbyuhrDf"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v — отказ редиректа не должен ронять сдачу", err)
	}
}

// Причина отказа сохраняется и доезжает до отчёта.
//
// Сборщик всегда говорит, почему цифр нет: «метрики отдельных постов
// для vk пока не поддержаны», «ролик не найден». Мы эту причину
// выбрасывали, и в кабинете «площадка не собирается», «ролик удалён» и
// «никто не посмотрел» выглядели одинаково — пустой цифрой. Первые два
// — наша работа, третье — работа креатора.
func TestCollectFailureKeepsReason(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://vk.com/clip-1_2"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	const reason = "Метрики отдельных постов для 'vk' пока не поддержаны"
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: {Platform: "vk", URL: url, Kind: "media", OK: false, Error: reason},
	}}
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo).WithCollector(fake)

	if _, err := svc.RunCollection(ctx, time.Now().UTC(), 50); err != nil {
		t.Fatalf("RunCollection: %v", err)
	}

	rep, err := repo.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.VideoRows) != 1 {
		t.Fatalf("строк в отчёте %d, ожидалась одна", len(rep.VideoRows))
	}
	row := rep.VideoRows[0]
	if row.CollectError != reason {
		t.Errorf("причина %q, ожидалась %q", row.CollectError, reason)
	}
	if row.CollectTriedAt == nil {
		t.Error("нет даты попытки — по ней отличают «сбор не дал цифр» от «сбор не ходил»")
	}
	if row.CollectedAt != nil {
		t.Error("дата сбора проставлена, хотя цифр не было: снимка с нулями быть не должно")
	}

	// А удачный обход причину стирает: она была про то, чего больше нет.
	fake.byURL[url] = okResult(url, 1200, 40, 3)
	// Своим расписанием ссылка уедет на сутки вперёд, поэтому зовём тот
	// путь, которым ходит открытая карточка.
	if _, err := svc.RefreshProject(ctx, projectID, nil, time.Now().UTC().Add(12*time.Minute), 25); err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	rep, err = repo.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report (после удачи): %v", err)
	}
	if rep.VideoRows[0].CollectError != "" {
		t.Errorf("причина осталась после удачного обхода: %q", rep.VideoRows[0].CollectError)
	}
	if rep.VideoRows[0].Views != 1200 {
		t.Errorf("просмотры %d, ожидалось 1200", rep.VideoRows[0].Views)
	}
}

// Отказ сборщика считается за попытку, а не за «ни разу не собрано».
//
// Самая дорогая находка ревью. Отметка свежести читалась из
// last_collected_at, а его пишет только успех: у ссылки, которую сборщик
// не умеет (VK, Likee, удалённый ролик), он оставался NULL навсегда.
// Значит, двойной запрос одного захода уезжал в платный обход дважды —
// и дороже всех обходились ровно те ссылки, которые заведомо ничего не
// вернут. Дебаунс смотрит на last_collect_try_at, который ставится до
// похода и потому не зависит от его исхода.
func TestRefreshCountsFailedAttemptAsFreshness(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://vk.com/clip-7_7"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: {Platform: "vk", URL: url, Kind: "media", OK: false,
			Error: "Метрики отдельных постов для 'vk' пока не поддержаны"},
	}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)

	now := time.Now().UTC()
	st, err := svc.RefreshProject(ctx, projectID, nil, now, 25)
	if err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	if st.NoData != 1 {
		t.Fatalf("первый проход: %+v, ожидался отказ сборщика", st)
	}

	// В пределах того же захода второго похода нет. Раньше он был — и так
	// при каждом F5, до бесконечности.
	st, err = svc.RefreshProject(ctx, projectID, nil, now.Add(5*time.Second), 25)
	if err != nil {
		t.Fatalf("RefreshProject (повтор): %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("повтор взял %d ссылок, ожидалось 0", st.Considered)
	}
	if fake.calls != 1 {
		t.Errorf("в instacurl сходили %d раза, ожидался один — отказ не считается за свежесть",
			fake.calls)
	}

	// А на следующем заходе — снова: отказ мог быть временным.
	st, err = svc.RefreshProject(ctx, projectID, nil, now.Add(12*time.Minute), 25)
	if err != nil {
		t.Fatalf("RefreshProject (по сроку): %v", err)
	}
	if st.Considered != 1 {
		t.Errorf("по сроку взято %d, ожидалась одна ссылка", st.Considered)
	}
}

// Креатор обновляет СВОИ ролики, а не весь проект.
//
// Чужие стоят кредитов, которых он не тратил, и сдвигают чужое
// расписание сбора. Доступ к ручке при этом у него законный — он
// участник проекта, — поэтому отказом тут не обойтись: нужна выборка.
func TestRefreshByCreatorTouchesOnlyOwnLinks(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:2],
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil || res.Created != 2 {
		t.Fatalf("CreateBatch: created=%d err=%v", res.Created, err)
	}
	urls := map[uuid.UUID]string{
		creators[0]: "https://www.tiktok.com/@a/video/1001",
		creators[1]: "https://www.tiktok.com/@b/video/1002",
	}
	for _, p := range res.Items {
		if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
			PublicationID: p.ID, ActorUserID: *p.CreatorUserID,
			URLs: []string{urls[*p.CreatorUserID]},
		}); err != nil {
			t.Fatalf("SubmitLinks: %v", err)
		}
	}

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		urls[creators[0]]: okResult(urls[creators[0]], 10, 1, 0),
		urls[creators[1]]: okResult(urls[creators[1]], 20, 2, 0),
	}}
	svc = svc.WithCollector(fake)

	st, err := svc.RefreshProject(ctx, projectID, &creators[0], time.Now().UTC(), 25)
	if err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	if st.Considered != 1 || st.Saved != 1 {
		t.Fatalf("%+v, ожидалась ровно одна своя ссылка", st)
	}
	if len(fake.urlsSeen) != 1 || fake.urlsSeen[0] != urls[creators[0]] {
		t.Errorf("в сборщик ушло %v, ожидалась только своя ссылка", fake.urlsSeen)
	}
}

// Причина отказа сборщика заказчику не показывается.
//
// Это текст СТОРОННЕГО сервиса, написанный для нас: «ScrapeCreators
// 404», «кончились кредиты». Заказчику он говорит не про его ролики, а
// про то, чем и на какие деньги мы их считаем. Чистим в модели, а не
// прячем на фронте: тот же JSON уходит в выгрузку и чужому клиенту.
func TestClientReportHidesCollectError(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://vk.com/clip-8_8"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: {Platform: "vk", URL: url, Kind: "media", OK: false,
			Error: "ScrapeCreators 404: Account doesn't exist"},
	}}
	repo := publications.NewRepo(pool)
	if _, err := publications.NewService(repo).WithCollector(fake).
		RunCollection(ctx, time.Now().UTC(), 50); err != nil {
		t.Fatalf("RunCollection: %v", err)
	}

	rep, err := repo.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.VideoRows) == 0 || rep.VideoRows[0].CollectError == "" {
		t.Fatalf("менеджер должен видеть причину: %+v", rep.VideoRows)
	}

	forClient := rep.ForClient()
	for _, v := range forClient.VideoRows {
		if v.CollectError != "" || v.CollectTriedAt != nil {
			t.Errorf("заказчику уехала наша кухня: %q", v.CollectError)
		}
	}
	// И менеджерский отчёт при этом не испорчен: ForClient возвращает
	// копию, а не правит исходный.
	if rep.VideoRows[0].CollectError == "" {
		t.Error("ForClient затёр причину в исходном отчёте")
	}
}

// Длинную причину режем по буквам, а не по байтам.
//
// Кириллица двухбайтовая: срез по байту разрубает букву пополам,
// Postgres такую строку не принимает, MarkFailed возвращает ошибку — и
// сбор обрывает ВСЮ пачку. Остальные ссылки остаются без снимков и
// через десять минут оплачиваются заново.
func TestCollectErrorIsCutByRunes(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://likee.video/@u/video/9"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	long := strings.Repeat("ошибка сборщика ", 60) // заведомо длиннее потолка
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		url: {Platform: "likee", URL: url, Kind: "media", OK: false, Error: long},
	}}
	repo := publications.NewRepo(pool)
	if _, err := publications.NewService(repo).WithCollector(fake).
		RunCollection(ctx, time.Now().UTC(), 50); err != nil {
		t.Fatalf("RunCollection: %v", err)
	}

	rep, err := repo.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.VideoRows) != 1 {
		t.Fatalf("строк в отчёте %d", len(rep.VideoRows))
	}
	got := rep.VideoRows[0].CollectError
	if got == "" {
		t.Fatal("причина не сохранилась вовсе")
	}
	if !utf8.ValidString(got) {
		t.Errorf("битый UTF-8 в причине: %q", got)
	}
}

// Обход по заходу берёт САМЫЕ ЗАСТОЯВШИЕСЯ ссылки, а не самые новые.
//
// Лимит отрезает двадцать пять штук, и раньше он резал по дате сдачи:
// в выборку попадали новейшие, а уже свежие из них тут же отсеивались.
// У проекта с тремя десятками роликов старые ссылки не обновлялись
// никогда — до них просто не доходила очередь.
func TestRefreshTakesStalestFirst(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(1), pubDay(2)},
		CreatedBy:      creators[0],
	})
	if err != nil || res.Created != 2 {
		t.Fatalf("CreateBatch: created=%d err=%v", res.Created, err)
	}
	// Первая сдана раньше и давно не трогалась, вторая — только что.
	type pub struct {
		id  uuid.UUID
		url string
	}
	pubs := []pub{
		{res.Items[0].ID, "https://www.tiktok.com/@a/video/old"},
		{res.Items[1].ID, "https://www.tiktok.com/@a/video/new"},
	}
	for _, p := range pubs {
		if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
			PublicationID: p.id, ActorUserID: creators[0], URLs: []string{p.url},
		}); err != nil {
			t.Fatalf("SubmitLinks: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET submitted_at = now() - interval '20 days',
       last_collected_at = now() - interval '20 days'
WHERE publication_id = $1`, pubs[0].id); err != nil {
		t.Fatalf("состарить: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE publication_links SET submitted_at = now(), last_collected_at = now()
WHERE publication_id = $1`, pubs[1].id); err != nil {
		t.Fatalf("освежить: %v", err)
	}

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		pubs[0].url: okResult(pubs[0].url, 10, 1, 0),
		pubs[1].url: okResult(pubs[1].url, 20, 2, 0),
	}}
	// Лимит в одну ссылку: место достанется тому, кто его заслужил.
	st, err := svc.WithCollector(fake).RefreshProject(ctx, projectID, nil, time.Now().UTC(), 1)
	if err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	if st.Considered != 1 {
		t.Fatalf("%+v, ожидалась одна ссылка", st)
	}
	if len(fake.urlsSeen) != 1 || fake.urlsSeen[0] != pubs[0].url {
		t.Errorf("обошли %v, ожидалась застоявшаяся ссылка", fake.urlsSeen)
	}
}

// Снятая с обхода ссылка не берётся ни по заходу в карточку, ни фоном.
//
// Это и есть «после выставления счёта эти видео больше не
// просматривать»: кабинет открывают каждый день, и если бы заход тянул
// архив, снятие с обхода не значило бы ничего.
func TestParkedLinksAreNotRefreshed(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/parked1"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	if _, err := pool.Exec(ctx, `
UPDATE publication_links l SET next_collect_at = 'infinity'::timestamptz
FROM project_publications p
WHERE p.id = l.publication_id AND p.project_id = $1`, projectID); err != nil {
		t.Fatalf("снять с обхода: %v", err)
	}

	fake := &fakeCollector{byURL: map[string]instacurl.Result{url: okResult(url, 10, 1, 0)}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)

	st, err := svc.RefreshProject(ctx, projectID, nil, time.Now().UTC(), 25)
	if err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("заход в карточку взял %d снятых ссылок", st.Considered)
	}

	// И форсированный обход тоже: он для ЗАКРЫВАЕМОГО периода, а не для
	// уже закрытых.
	fst, err := svc.ForceRefreshProject(ctx, projectID, time.Now().UTC())
	if err != nil {
		t.Fatalf("ForceRefreshProject: %v", err)
	}
	if fst.Considered != 0 {
		t.Errorf("форсированный обход взял %d снятых ссылок", fst.Considered)
	}
	if fake.calls != 0 {
		t.Errorf("в instacurl сходили %d раз по снятым ссылкам", fake.calls)
	}
}

// Форсированный обход не спрашивает о свежести — и не ходит по кругу.
//
// Он идёт пачками, пока ссылки не кончатся. Порог «ещё не тронут» у него
// — момент начала прохода, поэтому обойдённое в этом же проходе второй
// раз не берётся: иначе пачка возвращала бы одно и то же до упора в
// потолок пачек.
func TestForceRefreshIgnoresDebounceButNotItself(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const url = "https://www.tiktok.com/@u/video/forced1"
	projectID, _, cleanup := setupSubmittedLinks(t, url)
	defer cleanup()

	fake := &fakeCollector{byURL: map[string]instacurl.Result{url: okResult(url, 500, 5, 1)}}
	svc := publications.NewService(publications.NewRepo(pool)).WithCollector(fake)

	now := time.Now().UTC()
	// Только что обошли по заходу — дебаунс закрыт.
	if _, err := svc.RefreshProject(ctx, projectID, nil, now, 25); err != nil {
		t.Fatalf("RefreshProject: %v", err)
	}
	before := fake.calls

	// Форсированный идёт всё равно: срез снимается по сегодняшним цифрам,
	// а не по тем, что успели собраться.
	st, err := svc.ForceRefreshProject(ctx, projectID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ForceRefreshProject: %v", err)
	}
	if st.Saved == 0 {
		t.Errorf("форсированный обход ничего не собрал: %+v", st)
	}
	if fake.calls != before+1 {
		t.Errorf("походов в сборщик %d, ожидался один форсированный после %d", fake.calls, before)
	}
}
