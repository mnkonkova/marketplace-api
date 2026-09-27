package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// seedStats — сдать ссылки и записать по ним снимки за указанные дни.
// days: смещение от сегодня → просмотры.
func seedStats(t *testing.T, projectID, creator uuid.UUID, urls []string, days map[int]int64) {
	t.Helper()
	pool := integration.Pool(t)
	ctx := context.Background()
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creator},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creator,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creator,
		URLs:          urls,
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	// Ссылки берём ИМЕННО этого проекта. Через DueForCollection нельзя:
	// он не ограничен проектом и в полном прогоне возвращает чужие —
	// тест начинал плавать в зависимости от того, что осталось от
	// соседей.
	links := linksOfCreator(t, projectID, creator)
	now := time.Now().UTC()
	for _, l := range links {
		for offset, views := range days {
			v := views
			likes := views / 10
			comments := views / 100
			if err := repo.SaveStats(ctx, l, &v, &likes, &comments, nil, nil,
				now.AddDate(0, 0, offset)); err != nil {
				t.Fatalf("SaveStats: %v", err)
			}
		}
	}
}

// Просмотры накопительные: итог — последний снимок каждой ссылки, а не
// сумма по дням. Ошибка здесь завышает отчёт кратно и заметна не сразу.
func TestReportTotalsUseLatestSnapshotNotSum(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@u/video/1"},
		map[int]int64{-2: 100, -1: 400, 0: 900})

	rep, err := publications.NewService(publications.NewRepo(pool)).
		Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	if rep.Views != 900 {
		t.Errorf("просмотров %d, ожидалось 900 — берётся последний снимок, а не сумма 1400", rep.Views)
	}
	// Прирост ЗА СУТКИ — с вчерашнего снимка: 900 сегодня против 400
	// вчера. Снимки в посеве идут подряд, поэтому вчерашний есть.
	if rep.Growth24h == nil {
		t.Fatal("прирост за сутки не посчитан, хотя вчерашний снимок есть")
	}
	if *rep.Growth24h != 500 {
		t.Errorf("прирост за сутки %d, ожидалось 500 (900 − 400)", *rep.Growth24h)
	}
	if rep.Videos != 1 {
		t.Errorf("роликов %d, ожидался 1", rep.Videos)
	}
	if rep.ERPercent == nil {
		t.Fatal("ER не посчитан")
	}
	// 90 лайков + 9 комментариев на 900 просмотров = 11%.
	if got := *rep.ERPercent; got < 10.9 || got > 11.1 {
		t.Errorf("ER %.2f%%, ожидалось около 11%%", got)
	}
	if rep.AsOf == nil {
		t.Error("нет даты последнего сбора")
	}
}

// Прирост «за сутки» считается ровно с вчерашнего снимка.
//
// Собирают не каждый день, и разница с позавчерашним под подписью «за
// сутки» — неправда вдвое. Нет вчерашнего снимка — прирост НЕИЗВЕСТЕН,
// и это не ноль: ноль читается как «ролик встал».
func TestReportGrowthUnknownWithoutYesterday(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	// Снимки через день: сегодняшний есть, вчерашнего нет.
	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@u/video/1"},
		map[int]int64{-2: 400, 0: 900})

	rep, err := publications.NewService(publications.NewRepo(pool)).
		Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Views != 900 {
		t.Errorf("просмотров %d, ожидалось 900", rep.Views)
	}
	if rep.Growth24h != nil {
		t.Errorf("прирост за сутки %d, а вчерашнего снимка нет — должно быть «неизвестно»",
			*rep.Growth24h)
	}
	if len(rep.VideoRows) != 1 {
		t.Fatalf("строк роликов %d, ожидалась одна", len(rep.VideoRows))
	}
	if rep.VideoRows[0].Growth24h != nil {
		t.Errorf("в строке ролика прирост %d, ожидалось «неизвестно»", *rep.VideoRows[0].Growth24h)
	}
}

// Один ролик на пяти площадках — это ОДИН ролик, а не пять. И площадки
// сравниваются между собой: какая тянет.
func TestReportCountsVideosNotLinks(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	seedStats(t, projectID, creators[0], []string{
		"https://www.tiktok.com/@u/video/1",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://www.instagram.com/reel/C8xYzAbCdEf/",
	}, map[int]int64{0: 300})

	rep, err := publications.NewService(publications.NewRepo(pool)).
		Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Videos != 1 {
		t.Errorf("роликов %d, ожидался 1: три ссылки — это один ролик на трёх площадках", rep.Videos)
	}
	if rep.Views != 900 {
		t.Errorf("просмотров %d, ожидалось 900 — сумма по трём площадкам", rep.Views)
	}
	if len(rep.ByPlatform) != 3 {
		t.Fatalf("площадок в разбивке %d, ожидалось 3", len(rep.ByPlatform))
	}
	// Порядок фиксирован — он же порядок колонок в интерфейсе.
	if rep.ByPlatform[0].Platform != publications.PlatformTikTok {
		t.Errorf("первая площадка %s, ожидался tiktok", rep.ByPlatform[0].Platform)
	}
	for _, p := range rep.ByPlatform {
		if p.Views != 300 {
			t.Errorf("%s: %d просмотров, ожидалось 300", p.Platform, p.Views)
		}
	}
}

// Сравнение креаторов: сколько роликов, сколько просмотров, средний
// просмотр на ролик и доля в общем результате.
func TestReportCreatorComparison(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@a/video/1"}, map[int]int64{0: 750})
	seedStats(t, projectID, creators[1],
		[]string{"https://www.tiktok.com/@b/video/2"}, map[int]int64{0: 250})

	rep, err := publications.NewService(publications.NewRepo(pool)).
		Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.ByCreator) != 2 {
		t.Fatalf("креаторов в сравнении %d, ожидалось 2", len(rep.ByCreator))
	}
	byID := map[uuid.UUID]publications.CreatorRow{}
	for _, c := range rep.ByCreator {
		byID[c.CreatorUserID] = c
	}
	first := byID[creators[0]]
	if first.Views != 750 {
		t.Errorf("у первого %d просмотров, ожидалось 750", first.Views)
	}
	if first.Videos != 1 || first.AvgViews != 750 {
		t.Errorf("роликов %d, средний просмотр %.0f; ожидалось 1 и 750", first.Videos, first.AvgViews)
	}
	if got := first.SharePercent; got < 74.9 || got > 75.1 {
		t.Errorf("доля первого %.1f%%, ожидалось 75%%", got)
	}
}

// Креатор видит отчёт только по своим роликам. Это граница доступа:
// чужие цифры не должны попадать в выдачу вообще.
func TestReportCreatorSeesOnlyOwn(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@a/video/1"}, map[int]int64{0: 750})
	seedStats(t, projectID, creators[1],
		[]string{"https://www.tiktok.com/@b/video/2"}, map[int]int64{0: 250})

	svc := publications.NewService(publications.NewRepo(pool))
	rep, err := svc.Report(ctx, projectID, publications.ReportFilter{CreatorUserID: &creators[1]})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Views != 250 {
		t.Errorf("в своём срезе %d просмотров, ожидалось 250", rep.Views)
	}
	if len(rep.ByCreator) != 1 {
		t.Fatalf("в срезе креатора %d строк сравнения, ожидалась одна — своя", len(rep.ByCreator))
	}
	if rep.ByCreator[0].CreatorUserID != creators[1] {
		t.Error("в свой срез попал чужой креатор")
	}
	for _, v := range rep.VideoRows {
		if (v.CreatorUserID == nil || *v.CreatorUserID != creators[1]) {
			t.Errorf("в таблице роликов чужая строка: %s", v.URL)
		}
	}
}

// Пропущенный день не обваливает график: на каждую дату берётся
// последний известный снимок, а не снимок ровно этого дня.
func TestReportDailyGraphSurvivesMissingDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	// Две ссылки: у одной есть снимок за вчера, у другой — только за
	// позавчера и сегодня. Вчерашняя точка не должна просесть.
	seedStats(t, projectID, creators[0],
		[]string{"https://www.tiktok.com/@a/video/1"}, map[int]int64{-2: 100, -1: 200, 0: 300})
	seedStats(t, projectID, creators[1],
		[]string{"https://www.tiktok.com/@b/video/2"}, map[int]int64{-2: 500, 0: 700})

	rep, err := publications.NewService(publications.NewRepo(pool)).
		Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.ByDay) != 3 {
		t.Fatalf("точек графика %d, ожидалось 3: %+v", len(rep.ByDay), rep.ByDay)
	}
	// Вчера: 200 у первой + последний известный 500 у второй.
	if got := rep.ByDay[1].Views; got != 700 {
		t.Errorf("вчерашняя точка %d, ожидалось 700 — пропуск дня не должен обнулять ролик", got)
	}
	// График накопительный: не убывает.
	for i := 1; i < len(rep.ByDay); i++ {
		if rep.ByDay[i].Views < rep.ByDay[i-1].Views {
			t.Errorf("график просел на %s: %d после %d",
				rep.ByDay[i].Date.Format("2006-01-02"), rep.ByDay[i].Views, rep.ByDay[i-1].Views)
		}
	}
}

// Проект без единого собранного снимка не должен показывать ER = 0%:
// «нет данных» и «нулевая вовлечённость» — разные вещи.
func TestReportWithoutDataHasNoFakeZeroER(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	rep, err := svc.Report(ctx, projectID, publications.ReportFilter{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.ERPercent != nil {
		t.Errorf("ER = %.2f%% при отсутствии просмотров — должно быть «нет данных»", *rep.ERPercent)
	}
	if rep.AsOf != nil {
		t.Error("есть дата сбора там, где ничего не собирали")
	}
}

// linksOfCreator — сданные ссылки конкретного креатора в конкретном
// проекте, в виде, пригодном для repo.SaveStats.
//
// Именно креатора, а не всего проекта: seedStats вызывается по разу на
// каждого, и выборка по проекту переписывала бы статистику соседа.
func linksOfCreator(t *testing.T, projectID, creatorID uuid.UUID) []publications.LinkToCollect {
	t.Helper()
	pool := integration.Pool(t)
	rows, err := pool.Query(context.Background(), `
SELECT l.id, l.publication_id, p.project_id, l.platform, l.url_canonical, l.submitted_at
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1 AND p.creator_user_id = $2
ORDER BY l.submitted_at`, projectID, creatorID)
	if err != nil {
		t.Fatalf("links of project: %v", err)
	}
	defer rows.Close()

	out := make([]publications.LinkToCollect, 0, 8)
	for rows.Next() {
		var l publications.LinkToCollect
		if err := rows.Scan(&l.LinkID, &l.PublicationID, &l.ProjectID,
			&l.Platform, &l.URL, &l.SubmittedAt); err != nil {
			t.Fatalf("scan link: %v", err)
		}
		out = append(out, l)
	}
	return out
}
