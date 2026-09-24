package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Отчёт по проекту — то, что заменяет ручную таблицу.
//
// Два правила, из которых следует вся форма запросов ниже:
//
//  1. Просмотры НАКОПИТЕЛЬНЫЕ. Снимок за день — это «всего просмотров на
//     эту дату», а не «просмотров за день». Поэтому итог считается по
//     последнему снимку каждой ссылки, а не суммой по дням: сумма
//     завысила бы результат кратно.
//  2. Прирост за сутки — разница двух последних снимков, а не последний
//     снимок. Пропущенный день не ломает подсчёт: берём два ближайших
//     имеющихся.
//
// У закрытого проекта, чей ряд уже схлопнут (28 дней после закрытия),
// разбивки по дням нет — есть только итоговый снимок. Отчёт в этом случае
// честно помечается Collapsed.

// ReportFilter — чей срез показываем. Пустой — весь проект.
type ReportFilter struct {
	// CreatorUserID — только ролики этого креатора. Так креатор видит
	// «по своим», не получая чужих цифр.
	CreatorUserID *uuid.UUID
}

// DayPoint — точка накопительного графика.
type DayPoint struct {
	Date  time.Time `json:"date"`
	Views int64     `json:"views"`
}

// CreatorRow — строка сравнения креаторов.
type CreatorRow struct {
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	Videos        int       `json:"videos"`
	Views         int64     `json:"views"`
	AvgViews      float64   `json:"avg_views"`
	// SharePercent — доля в общем результате проекта.
	SharePercent float64 `json:"share_percent"`
}

// PlatformRow — строка сравнения площадок: какая тянет.
type PlatformRow struct {
	Platform string `json:"platform"`
	Videos   int    `json:"videos"`
	Views    int64  `json:"views"`
	Likes    int64  `json:"likes"`
	Comments int64  `json:"comments"`
	// Shares — репосты. Пусто, если площадка их не отдаёт: ноль означал
	// бы «репостов нет», а это другое утверждение.
	Shares *int64 `json:"shares,omitempty"`
	// ERPercent/ERWithoutShares — вовлечённость площадки и признак, что
	// она посчитана без репостов.
	ERPercent       *float64 `json:"er_percent,omitempty"`
	ERWithoutShares bool     `json:"er_without_shares,omitempty"`
}

// VideoRow — строка таблицы роликов.
type VideoRow struct {
	PublicationID uuid.UUID `json:"publication_id"`
	LinkID        uuid.UUID `json:"link_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	Platform      string    `json:"platform"`
	URL           string    `json:"url"`
	SubmittedAt   time.Time `json:"submitted_at"`
	Views         int64     `json:"views"`
	Likes         int64     `json:"likes"`
	Comments      int64     `json:"comments"`
	Shares        *int64    `json:"shares,omitempty"`
	Growth24h     int64     `json:"growth_24h"`
	ERPercent     *float64  `json:"er_percent,omitempty"`
	// ERWithoutShares — вовлечённость посчитана без репостов: площадка
	// их не отдала или ролик собирали до того, как мы начали их писать.
	ERWithoutShares bool       `json:"er_without_shares,omitempty"`
	CollectedAt     *time.Time `json:"collected_at,omitempty"`
}

// Report — всё, что показывает страница отчёта.
type Report struct {
	ProjectID uuid.UUID `json:"project_id"`
	Views     int64     `json:"views"`
	Likes     int64     `json:"likes"`
	Comments  int64     `json:"comments"`
	Shares    *int64    `json:"shares,omitempty"`
	Videos    int       `json:"videos"`
	Growth24h int64     `json:"growth_24h"`
	ERPercent *float64  `json:"er_percent,omitempty"`
	// ERWithoutShares — хоть одна площадка, вошедшая в расчёт, репостов
	// не отдала. Показатель занижен, и сказать об этом обязаны.
	ERWithoutShares bool          `json:"er_without_shares,omitempty"`
	AsOf            *time.Time    `json:"as_of,omitempty"`
	Collapsed       bool          `json:"collapsed"`
	ByDay           []DayPoint    `json:"by_day"`
	ByCreator       []CreatorRow  `json:"by_creator"`
	ByPlatform      []PlatformRow `json:"by_platform"`
	VideoRows       []VideoRow    `json:"videos_table"`
}

// Report — собрать отчёт. Пять запросов вместо одного: каждый агрегат
// режется по-своему, и склеивать их в один SQL значило бы получить
// декартово произведение строк.
func (r *Repo) Report(ctx context.Context, projectID uuid.UUID, f ReportFilter) (Report, error) {
	// Пустые срезы, а не nil: Go отдаёт nil-срез как JSON null, и любой
	// потребитель, делающий by_creator.length, падает на проекте без
	// статистики — то есть на каждом новом. Отдельная строка вместо
	// «само разберётся» здесь стоит дёшево, а ловилось это только глазами
	// в браузере.
	out := Report{
		ProjectID:  projectID,
		ByDay:      []DayPoint{},
		ByCreator:  []CreatorRow{},
		ByPlatform: []PlatformRow{},
		VideoRows:  []VideoRow{},
	}

	// Схлопнутый проект: детальной истории нет, отдаём снимок.
	stats, err := r.Stats(ctx, projectID)
	if err != nil {
		return out, err
	}
	if stats.Collapsed {
		// Снимок хранится ОДНОЙ строкой на проект — разбивки по креаторам
		// в нём нет. Отдать его в ответ на запрос конкретного креатора
		// значило бы показать ему цифры всего проекта, включая чужие.
		if f.CreatorUserID != nil {
			return out, ErrCollapsedNoDetail
		}
		out.Views, out.Likes, out.Comments = stats.Views, stats.Likes, stats.Comments
		out.Videos, out.AsOf, out.Collapsed = stats.VideosCount, stats.AsOf, true
		// В итоговом снимке репостов нет: он сворачивался до того, как мы
		// начали их собирать, и достраивать их задним числом неоткуда.
		out.ERPercent, out.ERWithoutShares = erPercent(stats.Likes, stats.Comments, nil, stats.Views)
		return out, nil
	}

	rows, err := r.videoRows(ctx, projectID, f)
	if err != nil {
		return out, err
	}
	// Итоги ниже считаются по ВСЕМ строкам, а в ответ уедет свежая
	// часть — см. videoRowsLimit.
	out.VideoRows = rows

	byCreator := make(map[uuid.UUID]*CreatorRow, 4)
	creatorOrder := make([]uuid.UUID, 0, 4)
	byPlatform := make(map[string]*PlatformRow, len(AllPlatforms))
	seenPub := make(map[uuid.UUID]bool, len(rows))

	// Репосты копим отдельно от остальных чисел: у них есть третье
	// состояние — «неизвестно», и обычным int64 его не выразить.
	var totalShares sharesTotal
	platformShares := map[string]*sharesTotal{}

	for _, v := range rows {
		out.Views += v.Views
		out.Likes += v.Likes
		out.Comments += v.Comments
		totalShares.add(v.Shares)
		out.Growth24h += v.Growth24h
		if v.CollectedAt != nil && (out.AsOf == nil || v.CollectedAt.After(*out.AsOf)) {
			out.AsOf = v.CollectedAt
		}
		// Ролик — это выкладка, а не ссылка: один ролик идёт на пять
		// площадок, и считать его пять раз было бы враньём.
		if !seenPub[v.PublicationID] {
			seenPub[v.PublicationID] = true
			out.Videos++
		}

		c, ok := byCreator[v.CreatorUserID]
		if !ok {
			c = &CreatorRow{CreatorUserID: v.CreatorUserID}
			byCreator[v.CreatorUserID] = c
			creatorOrder = append(creatorOrder, v.CreatorUserID)
		}
		c.Views += v.Views

		p, ok := byPlatform[v.Platform]
		if !ok {
			p = &PlatformRow{Platform: v.Platform}
			byPlatform[v.Platform] = p
		}
		p.Views += v.Views
		p.Likes += v.Likes
		p.Comments += v.Comments
		p.Videos++
		ps, ok := platformShares[v.Platform]
		if !ok {
			ps = &sharesTotal{}
			platformShares[v.Platform] = ps
		}
		ps.add(v.Shares)
	}
	if len(out.VideoRows) > videoRowsLimit {
		out.VideoRows = out.VideoRows[:videoRowsLimit]
	}
	out.Shares = totalShares.value()
	out.ERPercent, out.ERWithoutShares = erPercent(out.Likes, out.Comments, out.Shares, out.Views)
	for platform, ps := range platformShares {
		p := byPlatform[platform]
		p.Shares = ps.value()
		p.ERPercent, p.ERWithoutShares = erPercent(p.Likes, p.Comments, p.Shares, p.Views)
	}

	// Ролики по креаторам считаем по выкладкам, иначе у каждого выйдет
	// впятеро больше, чем он снял.
	pubPerCreator, err := r.videosPerCreator(ctx, projectID, f)
	if err != nil {
		return out, err
	}
	for _, id := range creatorOrder {
		c := byCreator[id]
		c.Videos = pubPerCreator[id]
		if c.Videos > 0 {
			c.AvgViews = float64(c.Views) / float64(c.Videos)
		}
		if out.Views > 0 {
			c.SharePercent = float64(c.Views) / float64(out.Views) * 100
		}
		out.ByCreator = append(out.ByCreator, *c)
	}
	// Порядок площадок фиксирован — он же порядок колонок в интерфейсе.
	for _, name := range AllPlatforms {
		if p, ok := byPlatform[name]; ok {
			out.ByPlatform = append(out.ByPlatform, *p)
		}
	}

	byDay, err := r.viewsByDay(ctx, projectID, f)
	if err != nil {
		return out, err
	}
	out.ByDay = byDay

	// Имена — одним запросом на весь отчёт. Без них «Сравнение креаторов»
	// показывает столбец uuid.
	if err := r.nameRows(ctx, &out); err != nil {
		return out, err
	}
	return out, nil
}

// nameRows — проставить человеческие имена в строках отчёта.
func (r *Repo) nameRows(ctx context.Context, out *Report) error {
	ids := make([]uuid.UUID, 0, len(out.ByCreator)+len(out.VideoRows))
	for _, c := range out.ByCreator {
		ids = append(ids, c.CreatorUserID)
	}
	for _, v := range out.VideoRows {
		ids = append(ids, v.CreatorUserID)
	}
	names, err := r.resolveNames(ctx, ids)
	if err != nil {
		return err
	}
	for i := range out.ByCreator {
		out.ByCreator[i].CreatorName = names[out.ByCreator[i].CreatorUserID]
	}
	for i := range out.VideoRows {
		out.VideoRows[i].CreatorName = names[out.VideoRows[i].CreatorUserID]
	}
	return nil
}

// videoRowsLimit — сколько строк роликов УЕЗЖАЕТ в ответ.
//
// Именно уезжает, а не читается: из этих же строк складываются итоги
// отчёта — просмотры, лайки, ролики, разбор по креаторам и площадкам.
// Ограничить сам запрос значило бы посчитать итог годового проекта по
// пятистам самым свежим ссылкам из трёх с половиной тысяч, то есть
// по седьмой части работы, — и показать это число над графиком,
// который нарисован по всем.
//
// Поэтому читаем всё, складываем всё, а в ответ кладём свежую часть:
// тяжёлым в этом ответе был килобайтный JSON на ссылку, а не сам
// проход по индексу.
const videoRowsLimit = 500

// videoRows — по строке на сданную ссылку: последний снимок и прирост
// относительно предыдущего.
func (r *Repo) videoRows(ctx context.Context, projectID uuid.UUID, f ReportFilter) ([]VideoRow, error) {
	const q = `
SELECT p.id, l.id, p.creator_user_id, l.platform, l.url_canonical, l.submitted_at,
       COALESCE(cur.views, 0), COALESCE(cur.likes, 0), COALESCE(cur.comments, 0), cur.shares,
       GREATEST(COALESCE(cur.views, 0) - COALESCE(prev.views, COALESCE(cur.views, 0)), 0),
       cur.collected_at
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments, shares, collected_at, stat_date
    FROM video_stat_daily d WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC LIMIT 1
) cur ON TRUE
LEFT JOIN LATERAL (
    SELECT views FROM video_stat_daily d
    WHERE d.link_id = l.id AND d.stat_date < cur.stat_date
    ORDER BY d.stat_date DESC LIMIT 1
) prev ON TRUE
WHERE p.project_id = $1
  AND p.status <> 'cancelled'
  AND ($2::uuid IS NULL OR p.creator_user_id = $2)
ORDER BY l.submitted_at DESC`
	rows, err := r.db.Query(ctx, q, projectID, f.CreatorUserID)
	if err != nil {
		return nil, fmt.Errorf("report video rows: %w", err)
	}
	defer rows.Close()

	out := make([]VideoRow, 0, 32)
	for rows.Next() {
		var v VideoRow
		if err := rows.Scan(&v.PublicationID, &v.LinkID, &v.CreatorUserID, &v.Platform,
			&v.URL, &v.SubmittedAt, &v.Views, &v.Likes, &v.Comments, &v.Shares,
			&v.Growth24h, &v.CollectedAt); err != nil {
			return nil, fmt.Errorf("scan report row: %w", err)
		}
		v.ERPercent, v.ERWithoutShares = erPercent(v.Likes, v.Comments, v.Shares, v.Views)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repo) videosPerCreator(ctx context.Context, projectID uuid.UUID, f ReportFilter) (map[uuid.UUID]int, error) {
	const q = `
SELECT p.creator_user_id, COUNT(DISTINCT p.id)
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
WHERE p.project_id = $1
  AND p.status <> 'cancelled'
  AND ($2::uuid IS NULL OR p.creator_user_id = $2)
GROUP BY p.creator_user_id`
	rows, err := r.db.Query(ctx, q, projectID, f.CreatorUserID)
	if err != nil {
		return nil, fmt.Errorf("videos per creator: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int, 4)
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan videos per creator: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// viewsByDay — накопительный график: на каждую дату сумма последних
// известных просмотров по всем ссылкам проекта.
//
// Именно последних известных, а не снимков ровно этого дня: пропущенный
// день иначе обвалил бы график до нуля, хотя ролики никуда не делись.
//
// Раньше это делалось LATERAL-подзапросом на каждую пару «день × ссылка»,
// то есть числом обращений, равным произведению. Проект на 60 роликов в
// месяц за год — это 3600 ссылок × 365 дат, больше миллиона обращений на
// один отчёт. Теперь один проход по снимкам, а перенос последнего
// известного значения делается здесь: стоимость растёт как объём данных,
// а не как произведение.
// reportWindowDays — за какой срок рисуем поденный ряд.
//
// Ряд читается целиком в память и схлопывается максимум в столько же
// точек, сколько дней. Без границы годовой проект на 60 роликов в месяц
// — это 3600 ссылок × 365 дат, до 160 тысяч строк и внешняя сортировка
// на десятки мегабайт, и всё это на ПЯТИ ручках отчёта. Год покрывает
// любой разумный вопрос к графику; кому нужна вся история, у того есть
// выгрузка.
const reportWindowDays = 365

func (r *Repo) viewsByDay(ctx context.Context, projectID uuid.UUID, f ReportFilter) ([]DayPoint, error) {
	const q = `
SELECT d.stat_date, d.link_id, COALESCE(d.views, 0)
FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
JOIN project_publications p ON p.id = l.publication_id
WHERE p.project_id = $1
  AND d.stat_date >= current_date - $3::int
  AND p.status <> 'cancelled'
  AND ($2::uuid IS NULL OR p.creator_user_id = $2)
ORDER BY d.stat_date`
	rows, err := r.db.Query(ctx, q, projectID, f.CreatorUserID, reportWindowDays)
	if err != nil {
		return nil, fmt.Errorf("views by day: %w", err)
	}
	defer rows.Close()

	out := make([]DayPoint, 0, 32)
	// last — последнее известное значение по каждой ссылке. Ссылка,
	// не собранная в этот день, продолжает учитываться прежним числом.
	last := make(map[uuid.UUID]int64, 64)
	var (
		current time.Time
		started bool
		total   int64
	)
	flush := func() {
		if started {
			out = append(out, DayPoint{Date: current, Views: total})
		}
	}
	for rows.Next() {
		var (
			day    time.Time
			linkID uuid.UUID
			views  int64
		)
		if err := rows.Scan(&day, &linkID, &views); err != nil {
			return nil, fmt.Errorf("scan day point: %w", err)
		}
		day = truncateDay(day)
		if !started {
			current, started = day, true
		} else if !day.Equal(current) {
			flush()
			current = day
		}
		// Разница к предыдущему известному значению этой ссылки: так
		// сумма пересчитывается за одно действие, а не обходом карты.
		total += views - last[linkID]
		last[linkID] = views
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// erPercent — вовлечённость: (лайки + комментарии) / просмотры.
//
// Считаем от просмотров, а не от подписчиков: подписчики принадлежат
// аккаунту креатора, а отчёт — про ролики проекта. Nil, когда просмотров
// нет: делить на ноль и показывать 0% — разные вещи, и второе врёт.
// erPercent — вовлечённость: (лайки + комментарии + репосты) ÷ просмотры.
//
// shares == nil означает «репостов не знаем»: площадка их не отдала либо
// ролик собирали до того, как мы начали их сохранять. Считаем без них и
// возвращаем вторым значением признак — показатель занижен, и молчать об
// этом нельзя. Ноль репостов и отсутствие репостов — разные вещи, и
// разница видна ровно здесь.
func erPercent(likes, comments int64, shares *int64, views int64) (*float64, bool) {
	if views <= 0 {
		return nil, shares == nil
	}
	sum := likes + comments
	if shares != nil {
		sum += *shares
	}
	v := float64(sum) / float64(views) * 100
	return &v, shares == nil
}

// sharesTotal — сумма репостов по набору строк и признак «где-то их нет».
//
// Признак поднимается, если репостов нет ХОТЬ У ОДНОЙ площадки, вошедшей
// в расчёт: одна неизвестная площадка делает приблизительным весь итог.
type sharesTotal struct {
	sum     int64
	missing bool
}

func (s *sharesTotal) add(shares *int64) {
	if shares == nil {
		s.missing = true
		return
	}
	s.sum += *shares
}

// value — сумма для расчёта ER. nil, если хоть где-то репосты неизвестны:
// складывать известные с неизвестными и выдавать это за полное число —
// то же занижение, только спрятанное.
func (s sharesTotal) value() *int64 {
	if s.missing {
		return nil
	}
	v := s.sum
	return &v
}

// Report — отчёт по проекту.
func (s *Service) Report(ctx context.Context, projectID uuid.UUID, f ReportFilter) (Report, error) {
	return s.repo.Report(ctx, projectID, f)
}

// ClientCanSeeStats — проект принадлежит этому заказчику И статистика ему
// открыта. Две проверки одним запросом: разделять их значило бы дать
// возможность по коду ответа отличить «не ваш проект» от «ваш, но цифры
// закрыты».
func (r *Repo) ClientCanSeeStats(ctx context.Context, projectID, clientID uuid.UUID) (bool, error) {
	var ok bool
	// Тот же круг смотрящих, что у остальных клиентских ручек:
	// заказчик, ведущий менеджер, админ (см. ClientOwnsProject). Сам
	// выключатель показа статистики при этом остаётся в силе — если
	// менеджер закрыл цифры клиенту, «глазами заказчика» он и увидит
	// их закрытыми. В этом и смысл кнопки.
	err := r.db.QueryRow(ctx, `
SELECT p.client_sees_stats FROM projects p
WHERE p.id = $1 AND (
    p.client_user_id = $2
 OR p.assigned_to_user_id = $2
 OR EXISTS (SELECT 1 FROM users u WHERE u.id = $2 AND u.is_admin)
)`, projectID, clientID).Scan(&ok)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("check client stats access: %w", err)
	}
	return ok, nil
}

// CreatorInProject — креатор состоит в проекте и его оттуда не убирали.
func (r *Repo) CreatorInProject(ctx context.Context, projectID, creatorID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_creators
    WHERE project_id = $1 AND creator_user_id = $2 AND removed_at IS NULL
)`, projectID, creatorID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check creator membership: %w", err)
	}
	return exists, nil
}

func (s *Service) ClientCanSeeStats(ctx context.Context, projectID, clientID uuid.UUID) (bool, error) {
	return s.repo.ClientCanSeeStats(ctx, projectID, clientID)
}

func (s *Service) CreatorInProject(ctx context.Context, projectID, creatorID uuid.UUID) (bool, error) {
	return s.repo.CreatorInProject(ctx, projectID, creatorID)
}
