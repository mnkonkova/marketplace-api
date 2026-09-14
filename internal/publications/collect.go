package publications

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/instacurl"
)

// CollectInterval — через сколько дней трогать ролик снова, в зависимости
// от его возраста.
//
// График: 0–5 дней — каждый день, 6–14 — раз в два, 15–28 — раз в четыре,
// дальше раз в восемь. Свежий ролик набирает просмотры быстро, архив почти
// не меняется, а объём сбора иначе растёт линейно с каждым месяцем работы:
// 60 видео × 5 площадок ежедневно — это 300 обходов в сутки на одного
// креатора, и это только первый проект.
func CollectInterval(ageDays int) int {
	switch {
	case ageDays <= 5:
		return 1
	case ageDays <= 14:
		return 2
	case ageDays <= 28:
		return 4
	default:
		return 8
	}
}

// collectRetryDelay — на сколько отодвигается вся пачка, когда сервис
// сбора недоступен целиком. Пятнадцать минут: достаточно, чтобы очередь
// прокрутилась, и мало, чтобы не потерять день.
const collectRetryDelay = 15 * time.Minute

// CollectionRetention — сколько ещё собираем после закрытия проекта.
// По истечении ежедневный ряд схлопывается в итоговый снимок.
const CollectionRetention = 28 * 24 * time.Hour

// LinkToCollect — ссылка, которую пора обойти.
type LinkToCollect struct {
	LinkID        uuid.UUID
	PublicationID uuid.UUID
	ProjectID     uuid.UUID
	Platform      string
	URL           string
	SubmittedAt   time.Time
}

// claimLease — на сколько ссылка помечается «взятой в работу». Обход
// пачки занимает секунды, десяти минут хватает с большим запасом; если
// воркер умрёт посреди работы, ссылки вернутся в очередь сами.
const claimLease = 10 * time.Minute

// DueForCollection — ссылки, которые пора собрать, не больше limit за раз.
//
// Ссылки не просто выбираются, а ЗАНИМАЮТСЯ: тем же запросом им
// сдвигается next_collect_at на короткую аренду. Без этого два экземпляра
// воркера (а они сосуществуют несколько секунд при каждом деплое) брали
// бы одни и те же сорок ссылок и слали их в instacurl дважды. Запись
// идемпотентна и данные бы не пострадали — страдал бы счёт за кредиты.
//
// Здесь же держится жёсткое правило «один ролик — не чаще раза в сутки»:
// условие по last_collected_at проверяется ДО похода в instacurl, а не
// только уникальным ключом при записи.
//
// SKIP LOCKED — тот же приём, что в outbox-воркере: две транзакции не
// столкнутся на одной строке, а просто разберут разные.
func (r *Repo) DueForCollection(ctx context.Context, now time.Time, limit int) ([]LinkToCollect, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `
WITH claimed AS (
    SELECT l.id
    FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    JOIN projects pr ON pr.id = p.project_id
    WHERE l.next_collect_at <= $1
      AND (l.last_collected_at IS NULL OR l.last_collected_at::date < $1::date)
      AND p.status <> 'cancelled'
      AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)
    ORDER BY l.next_collect_at
    LIMIT $2
    FOR UPDATE OF l SKIP LOCKED
),
taken AS (
    UPDATE publication_links l
    SET next_collect_at = $1::timestamptz + $3::interval
    FROM claimed c
    WHERE l.id = c.id
    RETURNING l.id, l.publication_id, l.platform, l.url_canonical, l.submitted_at
)
SELECT t.id, t.publication_id, p.project_id, t.platform, t.url_canonical, t.submitted_at
FROM taken t
JOIN project_publications p ON p.id = t.publication_id`
	rows, err := r.db.Query(ctx, q, now, limit, claimLease)
	if err != nil {
		return nil, fmt.Errorf("list links due for collection: %w", err)
	}
	defer rows.Close()

	out := make([]LinkToCollect, 0, limit)
	for rows.Next() {
		var l LinkToCollect
		if err := rows.Scan(&l.LinkID, &l.PublicationID, &l.ProjectID,
			&l.Platform, &l.URL, &l.SubmittedAt); err != nil {
			return nil, fmt.Errorf("scan link due: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveStats — снимок метрик на дату плюс перенос ссылки на следующий обход.
//
// Повторный вызов за ту же дату перезаписывает снимок, а не плодит строки:
// PK (link_id, stat_date). Пропущенный день не искажает историю — просто
// не будет строки за этот день.
func (r *Repo) SaveStats(ctx context.Context, link LinkToCollect,
	views, likes, comments, shares *int64, publishedAt *time.Time, now time.Time) error {

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// shares пишем как есть, включая NULL: «площадка не отдала репосты» и
	// «репостов ноль» — разные ответы, и подставлять ноль вместо
	// неизвестного значит занижать вовлечённость молча.
	if _, err := tx.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, shares, collected_at)
VALUES ($1, $2::date, $3, $4, $5, $6, $7)
ON CONFLICT (link_id, stat_date) DO UPDATE
SET views = EXCLUDED.views,
    likes = EXCLUDED.likes,
    comments = EXCLUDED.comments,
    shares = EXCLUDED.shares,
    collected_at = EXCLUDED.collected_at`,
		link.LinkID, now, views, likes, comments, shares, now); err != nil {
		return fmt.Errorf("upsert daily stat: %w", err)
	}

	interval := CollectInterval(ageInDays(link.SubmittedAt, now))
	if _, err := tx.Exec(ctx, `
UPDATE publication_links
SET last_collected_at = $2::timestamptz,
    collect_interval_days = $3,
    -- Дата публикации пишется один раз и больше не перезаписывается:
    -- ролик не выходит дважды, а источник со временем начинает врать
    -- (меняет часовой пояс, отдаёт дату перезалива). Первое непустое
    -- значение и есть ответ; COALESCE тут именно про это, а не про
    -- "подставить что-нибудь".
    published_at = COALESCE(published_at, $4),
    -- Явные приведения обязательны. Без ::timestamptz Postgres не может
    -- выбрать оператор "+" (кандидатов несколько: date, time, timestamp)
    -- и выводит для параметра противоречивые типы — 42P08. А без
    -- make_interval пришлось бы писать ($3 || ' days'), где тот же
    -- параметр был бы и числом, и текстом.
    next_collect_at = $2::timestamptz + make_interval(days => $3)
WHERE id = $1`, link.LinkID, now, interval, publishedAt); err != nil {
		return fmt.Errorf("reschedule link: %w", err)
	}
	return tx.Commit(ctx)
}

// publishedAtFormats — в каком виде источник отдаёт дату публикации.
//
// Он присылает строку, и какой именно формат — известно только по живым
// данным: у каждой площадки своё, а сборщик отдаёт то, что нашёл. Поэтому
// список, а не один формат, и молчаливый отказ вместо ошибки: мусор в
// этом поле не должен ломать сбор метрик, ради которого мы и ходили.
var publishedAtFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parsePublishedAt — дата публикации из ответа сборщика. nil, если поля
// нет или оно не разбирается: «не знаем» честнее выдуманной даты, и
// колонка для того и nullable.
func parsePublishedAt(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Секунды эпохи — тоже встречающийся вид. Проверяем до форматов:
	// time.Parse на числе всё равно не сработает.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		// Отсекаем заведомую чушь: до 2000 года площадок не было, а
		// миллисекунды легко принять за секунды и уехать в 56-й век.
		if n > 946_684_800 && n < 4_102_444_800 {
			t := time.Unix(n, 0).UTC()
			return &t
		}
		return nil
	}
	for _, layout := range publishedAtFormats {
		if t, err := time.Parse(layout, raw); err == nil {
			t = t.UTC()
			// Тот же фильтр здравого смысла: нулевая дата и будущее
			// говорят о том, что разобрали не то.
			if t.Year() < 2000 || t.After(time.Now().AddDate(1, 0, 0)) {
				return nil
			}
			return &t
		}
	}
	return nil
}

// MarkFailed — сервис не отдал метрики (площадка не поддержана, ролик
// удалён, кончились кредиты у поставщика).
//
// Снимок не пишем — нулей в отчёте быть не должно, они врут сильнее, чем
// пропуск. Но следующий обход всё равно отодвигаем: иначе битая ссылка
// будет дёргаться на каждом тике и съест слоты у живых.
func (r *Repo) MarkFailed(ctx context.Context, link LinkToCollect, now time.Time) error {
	interval := CollectInterval(ageInDays(link.SubmittedAt, now))
	_, err := r.db.Exec(ctx, `
UPDATE publication_links
SET collect_interval_days = $2,
    next_collect_at = $3::timestamptz + make_interval(days => $2)
WHERE id = $1`, link.LinkID, interval, now)
	if err != nil {
		return fmt.Errorf("reschedule failed link: %w", err)
	}
	return nil
}

// StopCollectionAfter — проект закрыт: дособираем ещё retention и на этом
// всё. Ставится в момент закрытия проекта.
func (r *Repo) StopCollectionAfter(ctx context.Context, projectID uuid.UUID, closedAt time.Time, retention time.Duration) error {
	_, err := r.db.Exec(ctx,
		`UPDATE projects SET collection_stops_at = $2 WHERE id = $1`,
		projectID, closedAt.Add(retention))
	if err != nil {
		return fmt.Errorf("set collection stop: %w", err)
	}
	return nil
}

// CollapseFinished — по проектам, у которых срок сбора вышел, свернуть
// ежедневный ряд в итоговый снимок и удалить детальные строки.
//
// ВНИМАНИЕ: удаление необратимо, восстановить историю по дням будет
// неоткуда. Поэтому снимок пишется ПЕРВЫМ и в той же транзакции: если
// запись снимка упадёт, удаления не произойдёт.
//
// Итог считается по последнему снимку каждой ссылки — просмотры
// накопительные, поэтому суммировать дни было бы кратной ошибкой.
func (r *Repo) CollapseFinished(ctx context.Context, now time.Time) (int, error) {
	rows, err := r.db.Query(ctx, `
SELECT id FROM projects
WHERE collection_stops_at IS NOT NULL
  AND collection_stops_at <= $1
  AND NOT EXISTS (SELECT 1 FROM project_stat_summary s WHERE s.project_id = projects.id)
  -- Схлопывать можно только то, что уже подытожено: период
  -- подытоживается срезом просмотров по каждой площадке, и снимать его
  -- будет неоткуда, если ежедневный ряд удалить раньше.
  --
  -- Условие ставим на РОЛИКИ, а не на периоды: пока есть вышедший
  -- ролик, который не накрыт ни одним подытоженным периодом, историю
  -- держим. Проверять «нет открытых периодов» нельзя (текущий открыт
  -- всегда, ряд не схлопнулся бы никогда), а проверять только
  -- заведённые периоды мало: у проекта, которого фоновая задача ещё не
  -- касалась, их нет вовсе.
  --
  -- Схлопывание не отменяется, а ждёт: периоды закрытого проекта уже в
  -- прошлом, и подытог доберётся до них в ближайшие часы.
  AND NOT EXISTS (
      SELECT 1
      FROM project_publications p
      WHERE p.project_id = projects.id AND p.status <> 'cancelled'
        AND (SELECT MIN(COALESCE(l.published_at, l.submitted_at))::date
             FROM publication_links l WHERE l.publication_id = p.id) IS NOT NULL
        AND NOT EXISTS (
            SELECT 1 FROM project_periods pp
            WHERE pp.project_id = p.project_id AND pp.status = 'locked'
              AND (SELECT MIN(COALESCE(l2.published_at, l2.submitted_at))::date
                   FROM publication_links l2 WHERE l2.publication_id = p.id)
                  BETWEEN pp.starts_on AND pp.ends_on
        )
  )`, now)
	if err != nil {
		return 0, fmt.Errorf("list projects to collapse: %w", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0, 8)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan project to collapse: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	done := 0
	for _, id := range ids {
		if err := r.collapseOne(ctx, id, now); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

func (r *Repo) collapseOne(ctx context.Context, projectID uuid.UUID, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Снимок — первым. last_per_link берёт последнюю дату по каждой
	// ссылке: просмотры накопительные, сумма по дням завысила бы итог
	// в разы.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_stat_summary
    (project_id, views, likes, comments, videos_count, as_of, collapsed_at)
SELECT $1,
       COALESCE(SUM(v.views), 0),
       COALESCE(SUM(v.likes), 0),
       COALESCE(SUM(v.comments), 0),
       COUNT(DISTINCT p.id),
       $2::date,
       $2
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments
    FROM video_stat_daily d
    WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE
WHERE p.project_id = $1
ON CONFLICT (project_id) DO NOTHING`, projectID, now); err != nil {
		return fmt.Errorf("write project summary: %w", err)
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM video_stat_daily
WHERE link_id IN (
    SELECT l.id FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    WHERE p.project_id = $1
)`, projectID); err != nil {
		return fmt.Errorf("delete daily stats: %w", err)
	}

	// Снимаем с обхода: собирать больше нечего.
	if _, err := tx.Exec(ctx, `
UPDATE publication_links
SET next_collect_at = 'infinity'::timestamptz
WHERE publication_id IN (SELECT id FROM project_publications WHERE project_id = $1)`,
		projectID); err != nil {
		return fmt.Errorf("park links: %w", err)
	}
	return tx.Commit(ctx)
}

// ProjectStats — сводные цифры проекта для отчёта: либо живые, либо из
// итогового снимка, если ряд уже схлопнут.
type ProjectStats struct {
	ProjectID   uuid.UUID  `json:"project_id"`
	Views       int64      `json:"views"`
	Likes       int64      `json:"likes"`
	Comments    int64      `json:"comments"`
	VideosCount int        `json:"videos_count"`
	AsOf        *time.Time `json:"as_of,omitempty"`
	// Collapsed — цифры из снимка, детальной истории по дням больше нет.
	Collapsed bool `json:"collapsed"`
}

// Stats — итоги проекта. Всегда с датой последнего сбора: без неё
// возникает вопрос «почему цифра не изменилась за час».
func (r *Repo) Stats(ctx context.Context, projectID uuid.UUID) (ProjectStats, error) {
	out := ProjectStats{ProjectID: projectID}

	var (
		views, likes, comments *int64
		videos                 *int
		asOf                   *time.Time
	)
	err := r.db.QueryRow(ctx, `
SELECT views, likes, comments, videos_count, as_of
FROM project_stat_summary WHERE project_id = $1`, projectID).
		Scan(&views, &likes, &comments, &videos, &asOf)
	if err == nil {
		out.Collapsed = true
		out.Views, out.Likes, out.Comments = deref(views), deref(likes), deref(comments)
		if videos != nil {
			out.VideosCount = *videos
		}
		out.AsOf = asOf
		return out, nil
	}
	if err != pgx.ErrNoRows {
		return out, fmt.Errorf("read project summary: %w", err)
	}

	err = r.db.QueryRow(ctx, `
SELECT COALESCE(SUM(v.views), 0), COALESCE(SUM(v.likes), 0),
       COALESCE(SUM(v.comments), 0), COUNT(DISTINCT p.id), MAX(v.collected_at)
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments, collected_at
    FROM video_stat_daily d
    WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE
WHERE p.project_id = $1`, projectID).
		Scan(&out.Views, &out.Likes, &out.Comments, &out.VideosCount, &asOf)
	if err != nil {
		return out, fmt.Errorf("read live stats: %w", err)
	}
	out.AsOf = asOf
	return out, nil
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func ageInDays(from, to time.Time) int {
	d := truncateDay(to).Sub(truncateDay(from))
	days := int(d.Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// Collector — то, что умеет отдать метрики по ссылкам. Интерфейс, а не
// конкретный клиент: сбор — единственное место, где домен зависит от
// чужого сервиса, и в тестах его надо подменять, не поднимая instacurl.
type Collector interface {
	Collect(ctx context.Context, urls []string) ([]instacurl.Result, error)
}

// WithCollector — подключить сбор статистики. Без него RunCollection
// возвращает ErrCollectorNotSet: молча ничего не делать нельзя, иначе
// пустой отчёт выглядит как «роликов не смотрят».
func (s *Service) WithCollector(c Collector) *Service {
	s.collector = c
	return s
}

// CollectStats — что сделал один проход сбора.
type CollectStats struct {
	Considered int `json:"considered"`
	Saved      int `json:"saved"`
	// NoData — сервис ответил, но метрик не дал: площадка не поддержана,
	// ролик удалён, кончились кредиты. Это ожидаемый исход, а не сбой.
	NoData int `json:"no_data"`
	// ThresholdNotified — скольким клиентам ушло «ролик перешагнул порог».
	ThresholdNotified int `json:"threshold_notified"`
}

// RunCollection — один проход: взять просроченные ссылки, сходить за
// метриками, записать снимки.
//
// Одной пачкой, а не по ссылке: instacurl держит два параллельных слота,
// и дробить запрос на сорок мелких — верный способ упереться в его же
// rate limit.
func (s *Service) RunCollection(ctx context.Context, now time.Time, batchSize int) (CollectStats, error) {
	var st CollectStats
	if s.collector == nil {
		return st, ErrCollectorNotSet
	}

	links, err := s.repo.DueForCollection(ctx, now, batchSize)
	if err != nil {
		return st, err
	}
	st.Considered = len(links)
	if len(links) == 0 {
		return st, nil
	}

	// Один канонический адрес может принадлежать нескольким ссылкам: один
	// ролик сдали двое, или один креатор в двух выкладках. Список на
	// отправку дедуплицируем (лишний поход стоит кредит), а обратное
	// соответствие держим списком — иначе все ссылки, кроме последней,
	// никогда не получали бы статистику.
	urls := make([]string, 0, len(links))
	byURL := make(map[string][]LinkToCollect, len(links))
	for _, l := range links {
		if _, dup := byURL[l.URL]; !dup {
			urls = append(urls, l.URL)
		}
		byURL[l.URL] = append(byURL[l.URL], l)
	}

	results, err := s.collector.Collect(ctx, urls)
	if err != nil {
		ObserveCollectError(collectErrorReason(err))
		// Пачку отодвигаем на короткий backoff, не трогая интервал.
		// Наказывать ссылки за чужую недоступность неправильно, но и
		// оставлять их первыми в очереди нельзя: выборка идёт по
		// next_collect_at ASC, и без сдвига следующий тик возьмёт те же
		// сорок ссылок, снова упрётся в таймаут — сбор встанет навсегда
		// и молча.
		if derr := s.repo.DeferLinks(ctx, links, now.Add(collectRetryDelay)); derr != nil {
			return st, fmt.Errorf("collect: %w (и отложить не удалось: %v)", err, derr)
		}
		// Сервис недоступен целиком — расписание не двигаем, попробуем
		// на следующем тике. Двигать его здесь значило бы наказать
		// ссылки за чужую недоступность.
		return st, fmt.Errorf("collect: %w", err)
	}

	seen := make(map[uuid.UUID]bool, len(links))
	for _, res := range results {
		matched, ok := byURL[res.URL]
		if !ok {
			// Сервис вернул то, чего мы не просили. Игнорируем: писать
			// метрики в чужую ссылку хуже, чем не писать вовсе.
			continue
		}
		m, hasData := res.Metrics()
		for _, link := range matched {
			seen[link.LinkID] = true

			if !hasData {
				st.NoData++
				ObserveCollect(link.Platform, "no_data")
				if err := s.repo.MarkFailed(ctx, link, now); err != nil {
					return st, err
				}
				continue
			}
			if err := s.repo.SaveStats(ctx, link, m.Views, m.Likes, m.Comments, m.Shares,
				parsePublishedAt(m.PublishedAt), now); err != nil {
				return st, err
			}
			// Полнота, а не только успех: сервис мог вернуть лайки и
			// комментарии, но не просмотры — так бывает, когда меняется
			// вёрстка площадки и разбор одного поля отваливается.
			// Формально сбор удался, фактически в отчёте по этой площадке
			// будут нули.
			if m.Views == nil {
				ObserveCollect(link.Platform, "no_views")
			} else {
				ObserveCollect(link.Platform, "ok")
			}
			st.Saved++
		}
	}

	// Порог просмотров проверяем по выкладке, а не по ссылке: клиент
	// задаёт порог для ролика, а ролик — это пять площадок вместе.
	notified := make(map[uuid.UUID]bool, len(links))
	for _, l := range links {
		if !seen[l.LinkID] || notified[l.PublicationID] {
			continue
		}
		notified[l.PublicationID] = true
		// Ошибку уведомления намеренно не поднимаем выше: цифры уже
		// сохранены, и терять их из-за неотправленного письма нельзя.
		// Проблемы с очередью видны по outbox_* и своим алертам.
		if sent, err := s.repo.NotifyThresholdCrossed(ctx, l.ProjectID, l.PublicationID, now); err == nil && sent {
			st.ThresholdNotified++
		}
	}

	// По чему сервис не ответил вовсе — тоже отодвигаем, иначе эти ссылки
	// будут дёргаться на каждом тике и займут слоты у живых.
	for _, l := range links {
		if !seen[l.LinkID] {
			st.NoData++
			ObserveCollect(l.Platform, "no_data")
			if err := s.repo.MarkFailed(ctx, l, now); err != nil {
				return st, err
			}
		}
	}
	return st, nil
}

// RunCollapse — свернуть ряды по проектам, у которых срок сбора вышел.
func (s *Service) RunCollapse(ctx context.Context, now time.Time) (int, error) {
	return s.repo.CollapseFinished(ctx, now)
}

// Stats — итоги проекта для отчёта.
func (s *Service) Stats(ctx context.Context, projectID uuid.UUID) (ProjectStats, error) {
	return s.repo.Stats(ctx, projectID)
}

// collectErrorReason — во что превращается ошибка вызова для метрики.
// Разные причины чинятся по-разному: недоступность — это инфраструктура,
// отказ по ключу — конфигурация, лимит — наш собственный график сбора.
func collectErrorReason(err error) string {
	switch {
	case errors.Is(err, instacurl.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, instacurl.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, instacurl.ErrNotConfigured):
		return "not_configured"
	default:
		return "unreachable"
	}
}

// DeferLinks — отодвинуть обход пачки ссылок, не трогая их интервал.
//
// Нужен, когда сервис сбора недоступен целиком: интервал остаётся
// прежним (ролик не постарел), а очередь получает возможность
// прокрутиться дальше.
func (r *Repo) DeferLinks(ctx context.Context, links []LinkToCollect, until time.Time) error {
	if len(links) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.LinkID)
	}
	_, err := r.db.Exec(ctx,
		`UPDATE publication_links SET next_collect_at = $2 WHERE id = ANY($1)`, ids, until)
	if err != nil {
		return fmt.Errorf("defer links: %w", err)
	}
	return nil
}
