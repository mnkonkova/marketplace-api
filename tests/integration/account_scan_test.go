package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/instacurl"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Обход аккаунтов креаторов: сервис сам находит новые ролики.
//
// Проверяется здесь не «находка записалась», а то, что обход не создаёт
// новых способов навредить. Он идёт каждый день, и любая ошибка
// повторяется каждый день: карточка, которая плодится; отказ «не мой»,
// который не держится; вопрос про ролик, который человек уже сдал
// своими руками. Каждый из этих случаев — свой тест.

// profileResult — ответ instacurl на адрес АККАУНТА: kind="profile" и
// список последних роликов. Именно так отвечает живой сервис (проверено
// запросом на профиль), и именно этим обход отличается от сбора
// статистики, где kind="media" и пост ровно один.
func profileResult(url, handle string, posts ...instacurl.PostMetrics) instacurl.Result {
	return instacurl.Result{
		Platform: "tiktok", URL: url, Handle: handle, Kind: "profile", OK: true,
		Posts: posts,
	}
}

func post(id, url string, published time.Time) instacurl.PostMetrics {
	views := int64(1000)
	return instacurl.PostMetrics{
		ID: id, URL: url, PublishedAt: published.Format(time.RFC3339), Views: &views,
	}
}

// setupScannedAccount — проект, креатор и его аккаунт, готовый к обходу.
func setupScannedAccount(t *testing.T, pool *pgxpool.Pool, accountURL string) (
	projectID uuid.UUID, creator uuid.UUID, svc *publications.Service, cleanup func()) {

	t.Helper()
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	svc = publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreatorAddAccount(ctx, projectID, creators[0], publications.AccountInput{
		Platform: "tiktok",
		Title:    "Рабочий",
		URL:      accountURL,
	}); err != nil {
		cleanup()
		t.Fatalf("CreatorAddAccount: %v", err)
	}
	return projectID, creators[0], svc, cleanup
}

// Обход кладёт находку — и только её: старый ролик с того же аккаунта
// находкой не становится.
//
// Профиль отдаёт последние десять постов независимо от возраста. Без
// ограничения по дате первый же обход завалил бы кабинет карточками про
// прошлогодний архив, среди которых потерялась бы та одна, что
// действительно ждёт ответа.
func TestAccountScanCreatesSuggestion(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const accountURL = "https://www.tiktok.com/@anya.kim"
	const freshURL = "https://www.tiktok.com/@anya.kim/video/7300000000000000101"
	const oldURL = "https://www.tiktok.com/@anya.kim/video/7300000000000000102"

	projectID, creator, svc, cleanup := setupScannedAccount(t, pool, accountURL)
	defer cleanup()

	now := time.Now().UTC()
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		accountURL: profileResult(accountURL, "@anya.kim",
			post("1", freshURL, now.Add(-24*time.Hour)),
			post("2", oldURL, now.AddDate(0, 0, -90)),
		),
	}}
	svc = svc.WithAccountScanner(fake)

	st, err := svc.RunAccountScan(ctx, now, 5)
	if err != nil {
		t.Fatalf("RunAccountScan: %v", err)
	}
	if st.Considered != 1 || st.Scanned != 1 {
		t.Fatalf("проход: %+v, ожидался один обойдённый аккаунт", st)
	}
	if st.Found != 1 {
		t.Fatalf("находок %d, ожидалась одна (свежий ролик): %+v", st.Found, st)
	}

	items, err := svc.CreatorSuggestions(ctx, creator)
	if err != nil {
		t.Fatalf("CreatorSuggestions: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("в кабинете %d находок, ожидалась одна", len(items))
	}
	if items[0].URL != freshURL {
		t.Errorf("нашли %q, ожидался свежий ролик %q", items[0].URL, freshURL)
	}
	if items[0].ProjectID != projectID {
		t.Errorf("находка ушла в проект %s вместо %s", items[0].ProjectID, projectID)
	}
	// Подпись автора нужна, чтобы человек опознал свой ролик, не
	// открывая ссылку.
	if items[0].AuthorHandle != "@anya.kim" {
		t.Errorf("подпись автора %q, ожидалась «@anya.kim»", items[0].AuthorHandle)
	}
}

// Повторный обход не плодит карточку — и вообще не ходит в сервис
// второй раз за сутки.
//
// Проверяем по числу походов, а не по числу строк в базе: деньги
// тратятся там. Уникальный индекс спас бы от дублей, но не от второго
// счёта за кредиты.
func TestAccountScanDoesNotDuplicateSuggestion(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const accountURL = "https://www.tiktok.com/@anya.kim2"
	const videoURL = "https://www.tiktok.com/@anya.kim2/video/7300000000000000201"

	_, creator, svc, cleanup := setupScannedAccount(t, pool, accountURL)
	defer cleanup()

	now := time.Now().UTC()
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		accountURL: profileResult(accountURL, "@anya.kim2", post("1", videoURL, now.Add(-time.Hour))),
	}}
	svc = svc.WithAccountScanner(fake)

	if _, err := svc.RunAccountScan(ctx, now, 5); err != nil {
		t.Fatalf("первый обход: %v", err)
	}

	// Тот же день — очередь не должна отдать аккаунт вовсе.
	st, err := svc.RunAccountScan(ctx, now.Add(3*time.Hour), 5)
	if err != nil {
		t.Fatalf("повтор в тот же день: %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("за сутки аккаунт взят %d раз, ожидался один", st.Considered+1)
	}
	if fake.calls != 1 {
		t.Errorf("в instacurl сходили %d раза, ожидался один — правило «раз в сутки» обойдено", fake.calls)
	}

	// Следующий день: аккаунт снова в очереди, ролик тот же — второй
	// карточки быть не должно.
	st, err = svc.RunAccountScan(ctx, now.Add(25*time.Hour), 5)
	if err != nil {
		t.Fatalf("обход назавтра: %v", err)
	}
	if st.Considered != 1 {
		t.Fatalf("назавтра аккаунт не взят: %+v", st)
	}
	if st.Found != 0 {
		t.Errorf("повторный обход завёл %d новых находок, ожидалось 0", st.Found)
	}

	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM publication_link_suggestions WHERE creator_user_id = $1`, creator).Scan(&n); err != nil {
		t.Fatalf("count suggestions: %v", err)
	}
	if n != 1 {
		t.Errorf("карточек %d, ожидалась одна", n)
	}
}

// «Не мой» держится. Обход идёт каждый день, и отвергнутая находка не
// должна возвращаться завтра той же карточкой — иначе кнопка «не мой»
// бесполезна, а кабинет превращается в ленту, которую нельзя закрыть.
func TestAccountScanDoesNotResurrectDismissed(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const accountURL = "https://www.tiktok.com/@anya.kim3"
	const videoURL = "https://www.tiktok.com/@anya.kim3/video/7300000000000000301"

	_, creator, svc, cleanup := setupScannedAccount(t, pool, accountURL)
	defer cleanup()

	now := time.Now().UTC()
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		accountURL: profileResult(accountURL, "@anya.kim3", post("1", videoURL, now.Add(-time.Hour))),
	}}
	svc = svc.WithAccountScanner(fake)

	if _, err := svc.RunAccountScan(ctx, now, 5); err != nil {
		t.Fatalf("первый обход: %v", err)
	}
	items, err := svc.CreatorSuggestions(ctx, creator)
	if err != nil || len(items) != 1 {
		t.Fatalf("после обхода находок %d (ошибка %v), ожидалась одна", len(items), err)
	}
	if err := svc.DismissSuggestion(ctx, items[0].ID, creator); err != nil {
		t.Fatalf("DismissSuggestion: %v", err)
	}

	st, err := svc.RunAccountScan(ctx, now.Add(25*time.Hour), 5)
	if err != nil {
		t.Fatalf("обход назавтра: %v", err)
	}
	if st.Found != 0 {
		t.Errorf("обход вернул отвергнутое: находок %d", st.Found)
	}
	items, err = svc.CreatorSuggestions(ctx, creator)
	if err != nil {
		t.Fatalf("CreatorSuggestions: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("отвергнутая находка вернулась: %+v", items)
	}
}

// Ролик, сданный руками, находкой не становится вовсе.
//
// Спрашивать «это ваш ролик?» про ссылку, которую человек сам только
// что вставил, — значит показать, что сервис не помнит, что ему отдали.
// Сверка идёт по КАНОНИЧЕСКОМУ адресу: площадка отдаёт ссылку с
// рекламным хвостом, а креатор сдавал её без хвоста, и посимвольное
// сравнение решило бы, что это разные ролики.
func TestAccountScanSkipsAlreadySubmittedLink(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const accountURL = "https://www.tiktok.com/@anya.kim4"
	const videoURL = "https://www.tiktok.com/@anya.kim4/video/7300000000000000401"

	projectID, creator, svc, cleanup := setupScannedAccount(t, pool, accountURL)
	defer cleanup()

	batch, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creator},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creator,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: batch.Items[0].ID,
		ActorUserID:   creator,
		URLs:          []string{videoURL},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	now := time.Now().UTC()
	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		accountURL: profileResult(accountURL, "@anya.kim4",
			post("1", videoURL+"?utm_source=app&_r=1", now.Add(-time.Hour))),
	}}
	svc = svc.WithAccountScanner(fake)

	st, err := svc.RunAccountScan(ctx, now, 5)
	if err != nil {
		t.Fatalf("RunAccountScan: %v", err)
	}
	if st.Found != 0 {
		t.Errorf("сданный ролик стал находкой: %+v", st)
	}
	if st.Skipped != 1 {
		t.Errorf("отсеяно %d, ожидался один сданный ролик: %+v", st.Skipped, st)
	}
	items, err := svc.CreatorSuggestions(ctx, creator)
	if err != nil {
		t.Fatalf("CreatorSuggestions: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("в кабинете появился вопрос про уже сданный ролик: %+v", items)
	}
}

// Обход выключен — значит выключен: ни похода в сервис, ни занятой
// очереди.
//
// Тихий no-op здесь недопустим: «обход выключен» и «обход сломан»
// должны различаться, иначе пустой кабинет креатора читается как «новых
// роликов нет».
func TestAccountScanDisabledGoesNowhere(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const accountURL = "https://www.tiktok.com/@anya.kim5"
	_, creator, svc, cleanup := setupScannedAccount(t, pool, accountURL)
	defer cleanup()

	st, err := svc.RunAccountScan(ctx, time.Now().UTC(), 5)
	if !errors.Is(err, publications.ErrAccountScannerNotSet) {
		t.Fatalf("без сканера ошибка %v, ожидалась ErrAccountScannerNotSet", err)
	}
	if st.Considered != 0 {
		t.Errorf("выключенный обход занял %d аккаунтов", st.Considered)
	}

	// Очередь не тронута: включённый завтра обход возьмёт аккаунт сразу,
	// а не будет ждать, пока истечёт чей-то лиз.
	var scanned *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_scanned_at FROM project_accounts WHERE creator_user_id = $1`,
		creator).Scan(&scanned); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if scanned != nil {
		t.Errorf("выключенный обход пометил аккаунт обойдённым: %v", scanned)
	}
}

// Проект отменён или сбор по нему остановлен — не обходим.
//
// Там и сданные ссылки уже не опрашиваются: тратить кредиты на поиск
// новых роликов по закрытому проекту незачем, а находка в отменённом
// проекте ещё и вредна — привязать её будет некуда.
func TestAccountScanSkipsStoppedProject(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	const accountURL = "https://www.tiktok.com/@anya.kim6"
	projectID, _, svc, cleanup := setupScannedAccount(t, pool, accountURL)
	defer cleanup()

	now := time.Now().UTC()
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET collection_stops_at = $2 WHERE id = $1`,
		projectID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("stop collection: %v", err)
	}

	fake := &fakeCollector{byURL: map[string]instacurl.Result{
		accountURL: profileResult(accountURL, "@anya.kim6",
			post("1", "https://www.tiktok.com/@anya.kim6/video/7300000000000000601", now)),
	}}
	svc = svc.WithAccountScanner(fake)

	st, err := svc.RunAccountScan(ctx, now, 5)
	if err != nil {
		t.Fatalf("RunAccountScan: %v", err)
	}
	if st.Considered != 0 {
		t.Errorf("обойдён аккаунт проекта, по которому сбор остановлен: %+v", st)
	}
	if fake.calls != 0 {
		t.Errorf("в instacurl сходили %d раз по остановленному проекту", fake.calls)
	}
}

// Обход снимает число подписчиков — даже когда роликов не нашёл.
//
// Доплата за подписчиков считается с прироста за период, а площадка
// отдаёт только «сколько сейчас». Без истории прирост не отличить от
// того, что человек набрал до проекта: менеджер вписывал число руками, и
// проверить его было нечем.
//
// Снимок кладётся ДО проверки на пустой список постов: у личной страницы
// VK посты собираются, а аудитория скрыта, и наоборот тоже бывает.
func TestAccountScanSnapshotsFollowers(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	const url = "https://www.tiktok.com/@subs.scan"
	_, _, svc, cleanup := setupScannedAccount(t, pool, url)
	defer cleanup()

	followers := int64(12_300)
	res := profileResult(url, "subs.scan") // роликов нет вовсе
	res.Followers = &followers
	fake := &fakeCollector{byURL: map[string]instacurl.Result{url: res}}
	if _, err := svc.WithAccountScanner(fake).
		RunAccountScan(ctx, time.Now().UTC(), 10); err != nil {
		t.Fatalf("RunAccountScan: %v", err)
	}

	var got int64
	if err := pool.QueryRow(ctx, `
SELECT d.followers FROM account_follower_daily d
JOIN project_accounts a ON a.id = d.account_id
WHERE a.url = $1`, url).Scan(&got); err != nil {
		t.Fatalf("снимок не записан: %v", err)
	}
	if got != followers {
		t.Errorf("подписчиков в снимке %d, ожидалось %d", got, followers)
	}
}

// Площадка не отдала подписчиков — снимка нет, а не ноль.
//
// Ноль бывает у нового аккаунта, и писать его на месте «не измерили»
// значило бы считать прирост от выдуманного нуля: следующий снимок дал
// бы прирост размером во всю аудиторию человека.
func TestAccountScanSkipsUnknownFollowers(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	const url = "https://www.tiktok.com/@subs.hidden"
	_, _, svc, cleanup := setupScannedAccount(t, pool, url)
	defer cleanup()

	res := profileResult(url, "subs.hidden", post("p1", url+"/video/1", time.Now()))
	res.Followers = nil
	fake := &fakeCollector{byURL: map[string]instacurl.Result{url: res}}
	if _, err := svc.WithAccountScanner(fake).
		RunAccountScan(ctx, time.Now().UTC(), 10); err != nil {
		t.Fatalf("RunAccountScan: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM account_follower_daily d
JOIN project_accounts a ON a.id = d.account_id
WHERE a.url = $1`, url).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("записано %d снимков — «не измерили» превратилось в число", rows)
	}
}

// Очередь обхода аккаунтов открывается в НОЛЬ ЧАСОВ, а не «через сутки».
//
// Решение владельца от 2 октября: «подписчиков раз в день в 00:00».
// Разница не косметическая. Срез подписчиков — основа доплаты за
// прирост, а прирост считается по границам периода, то есть по ДАТАМ. От
// «+24 часа» аккаунт уплывает по времени суток: обошли в 23:50 — следующий
// раз в 23:50 следующего дня, и в один календарный день снимка нет
// вовсе, а в соседний попадают два. На границе периода это ровно та
// разница, по которой человеку платят.
func TestAccountScanRunsAtMidnight(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	const url = "https://www.tiktok.com/@midnight.scan"
	_, _, svc, cleanup := setupScannedAccount(t, pool, url)
	defer cleanup()

	// Обход прошёл поздним вечером.
	evening := time.Now().UTC().Truncate(24 * time.Hour).Add(23*time.Hour + 50*time.Minute)
	followers := int64(1000)
	res := profileResult(url, "midnight.scan")
	res.Followers = &followers
	fake := &fakeCollector{byURL: map[string]instacurl.Result{url: res}}
	if _, err := svc.WithAccountScanner(fake).RunAccountScan(ctx, evening, 10); err != nil {
		t.Fatalf("RunAccountScan: %v", err)
	}

	var next time.Time
	if err := pool.QueryRow(ctx,
		`SELECT next_scan_at FROM project_accounts WHERE url = $1`, url).Scan(&next); err != nil {
		t.Fatalf("read next_scan_at: %v", err)
	}
	midnight := evening.Truncate(24*time.Hour).AddDate(0, 0, 1)
	if !next.UTC().Equal(midnight) {
		t.Errorf("следующий обход в %s, ожидалась полночь %s", next.UTC(), midnight)
	}
	// И это РАНЬШЕ, чем «через сутки»: иначе снимок уехал бы в
	// послезавтра и один календарный день остался бы без него.
	if !next.UTC().Before(evening.Add(24 * time.Hour)) {
		t.Error("полночь оказалась позже суток от обхода — расчёт даты сломан")
	}
}
