package billing

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
)

// Дашборд заказчика: то же /me/overview, но с окном.
//
// Окна СКОЛЬЗЯЩИЕ, а не календарные: дашборд смотрят в любой день, и
// «месяц» первого числа, показывающий один день работы, продавал бы
// хуже, чем честные последние тридцать дней.
//
// Правило, общее для всего блока: ИТОГИ — за всё время, ПРИРОСТЫ — окно
// против предыдущего окна такой же длины. Итоги здесь те же, что и в
// остальной сводке (по ним же считается стоимость тысячи), и подменять
// их оконными значило бы тихо поменять смысл полей, которые уже есть.
// Ряд по дням и «лучшие ролики» — наоборот, чисто оконные: это сам
// период наблюдения.

const (
	RangeWeek    = "week"
	RangeMonth   = "month"
	RangeQuarter = "quarter"
)

// rangeDays — длина окна в днях, включая сегодня.
var rangeDays = map[string]int{
	RangeWeek:    7,
	RangeMonth:   30,
	RangeQuarter: 90,
}

// RangeValues — допустимые значения параметра, для сообщения об ошибке.
var RangeValues = []string{RangeWeek, RangeMonth, RangeQuarter}

// topVideosLimit — сколько роликов показываем в «лучшем за окно».
// Три: это витрина, а не отчёт.
const topVideosLimit = 3

// shortMonths — русские сокращения для подписи окна. Родительный падеж:
// подпись всегда идёт с числом («17 авг.»).
var shortMonths = [...]string{
	"янв.", "февр.", "марта", "апр.", "мая", "июня",
	"июля", "авг.", "сент.", "окт.", "нояб.", "дек.",
}

// dashWindow — окно наблюдения и предыдущее такой же длины.
type dashWindow struct {
	Name     string
	From, To time.Time
	// PrevFrom/PrevTo — окно, с которым сравниваем прирост. Такой же
	// длины и вплотную перед текущим.
	PrevFrom, PrevTo time.Time
	Label            string
}

// newDashWindow — окно по имени диапазона. ok=false у незнакомого
// значения: молча подставить месяц значило бы показать человеку не то,
// что он просил, и не сказать об этом.
func newDashWindow(name string, now time.Time) (dashWindow, bool) {
	days, ok := rangeDays[name]
	if !ok {
		return dashWindow{}, false
	}
	to := truncDay(now)
	from := to.AddDate(0, 0, -(days - 1))
	prevTo := from.AddDate(0, 0, -1)
	prevFrom := prevTo.AddDate(0, 0, -(days - 1))
	return dashWindow{
		Name: name, From: from, To: to,
		PrevFrom: prevFrom, PrevTo: prevTo,
		Label: rangeLabel(from, to),
	}, true
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// rangeLabel — «17 авг. — 15 сент. 2026». Год у начала окна пишем только
// когда окно его пересекает: в обычном случае он лишний шум.
func rangeLabel(from, to time.Time) string {
	left := fmt.Sprintf("%d %s", from.Day(), shortMonths[int(from.Month())-1])
	if from.Year() != to.Year() {
		left = fmt.Sprintf("%s %d", left, from.Year())
	}
	return fmt.Sprintf("%s — %d %s %d", left, to.Day(), shortMonths[int(to.Month())-1], to.Year())
}

// deltaPct — прирост в процентах против предыдущего окна.
//
// nil, если сравнивать не с чем. Ноль и отсутствие здесь означают
// РАЗНОЕ: ноль — «не выросло», отсутствия — «нет прошлого окна». На
// продающем экране путать их нельзя, поэтому поле необязательное, а не
// нулевое.
func deltaPct(cur, prev int64) *int {
	if prev <= 0 {
		return nil
	}
	v := int(math.Round(float64(cur-prev) / float64(prev) * 100))
	return &v
}

// OverviewEngagement — взаимодействия: лайки, комментарии и репосты.
//
// Признак «репостов нет» живёт в соседнем блоке er: он один на оба
// числа, потому что причина одна — площадка их не отдала.
type OverviewEngagement struct {
	// Total/Comments — за всё время, как и остальные итоги сводки.
	// Приростов здесь нет намеренно: прирост окна рядом с итогом за всё
	// время описывал бы не то число, возле которого стоит. Оконные
	// значения и их приросты — в блоке window.
	Total    int64 `json:"total"`
	Comments int64 `json:"comments"`
}

// OverviewER — вовлечённость: (лайки + комментарии + репосты) ÷ просмотры.
type OverviewER struct {
	// Percent — за всё время. Изменение вовлечённости — в блоке window,
	// вместе с оконной вовлечённостью, которую оно и описывает.
	Percent float64 `json:"percent"`
	// WithoutShares — хотя бы одна площадка не отдаёт репосты, и они в
	// расчёт не вошли. Ноль репостов и «мы их не знаем» — разные
	// утверждения; на экране это звёздочка.
	WithoutShares bool `json:"without_shares,omitempty"`
}

// OverviewWindow — числа ЗА ОКНО, своими именами.
//
// Отдельный блок, а не приросты рядом с общими итогами, по простой
// причине: заголовок дашборда и подпись «+34% к прошлому месяцу»
// обязаны описывать ОДНУ И ТУ ЖЕ величину. «3 млн просмотров, +34% к
// прошлому месяцу» при итоге за всё время читается как «3 млн за
// месяц» — экран врёт даже при верных числах.
//
// Поэтому: здесь всё оконное, а views.total, engagement и er выше —
// за всё время, и приростов при них нет вовсе.
type OverviewWindow struct {
	Views int64 `json:"views"`
	// ViewsDeltaPct — против предыдущего окна такой же длины. Поля нет
	// вовсе, если сравнивать не с чем: ноль означал бы «не выросло».
	ViewsDeltaPct *int `json:"views_delta_pct,omitempty"`
	// Engagement — лайки + комментарии + репосты, набранные за окно.
	Engagement         int64 `json:"engagement"`
	EngagementDeltaPct *int  `json:"engagement_delta_pct,omitempty"`
	Comments           int64 `json:"comments"`
	CommentsDeltaPct   *int  `json:"comments_delta_pct,omitempty"`
	// ERPercent — вовлечённость окна: взаимодействия окна ÷ просмотры
	// окна. Ноль, пока просмотров в окне нет.
	ERPercent float64 `json:"er_percent"`
	// ERDeltaPP — изменение в ПУНКТАХ против предыдущего окна: «было
	// 4,7 — стало 4,4» понятнее, чем «упало на 6%».
	ERDeltaPP *float64 `json:"er_delta_pp,omitempty"`
	// ERWithoutShares — в окне хотя бы одна площадка не отдала репосты.
	ERWithoutShares bool `json:"er_without_shares,omitempty"`
}

// OverviewPlatform — площадка на дашборде.
type OverviewPlatform struct {
	Platform string `json:"platform"`
	// Views — за всё время; доля считается от них же, поэтому доли пяти
	// площадок дают сто процентов (с точностью до округления).
	Views    int64 `json:"views"`
	SharePct int   `json:"share_pct"`
	// WindowViews/WindowSharePct — то же за окно. Полоса состава на
	// дашборде рисуется по ним, чтобы сходиться с главным числом;
	// оконные доли дают сто процентов от оконной суммы.
	WindowViews    int64 `json:"window_views"`
	WindowSharePct int   `json:"window_share_pct"`
	// ERPercent — вовлечённость площадки. nil, пока просмотров нет.
	ERPercent       *float64 `json:"er_percent,omitempty"`
	ERWithoutShares bool     `json:"er_without_shares,omitempty"`
	// DeltaPct — прирост просмотров за окно против предыдущего окна.
	DeltaPct *int `json:"delta_pct,omitempty"`
	// Series — прирост по дням внутри окна. Дни без сбора пропущены, а
	// не отданы нулём: ноль нарисовал бы провал там, где просто не
	// приходил сборщик.
	Series []OverviewPoint `json:"series"`
}

// OverviewTopVideo — ролик из «лучшего за окно».
type OverviewTopVideo struct {
	PublicationID uuid.UUID `json:"publication_id"`
	// Title — название, которое дал креатор. Пустое отдаём как есть:
	// подставлять «Без названия» — дело интерфейса, а не сервера.
	Title string `json:"title"`
	// Platform — площадка-лидер: та, что дала больше всех просмотров за
	// окно. Ссылка — на неё же.
	Platform string `json:"platform"`
	// PublishedAt — когда ролик вышел, ГГГГ-ММ-ДД. Дата выхода, а не
	// сдачи ссылки: см. publications.ClientFeed.
	PublishedAt string `json:"published_at"`
	// Views — просмотры, набранные ЗА ОКНО. Показываем ту же величину,
	// по которой сортируем: иначе список едет относительно чисел.
	Views int64  `json:"views"`
	URL   string `json:"url"`
}

// OverviewMarket — во сколько раз рынок дороже нас.
type OverviewMarket struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// PricePer1000 — цена тысячи у ориентира, копейки.
	PricePer1000 int64 `json:"price_per_1000"`
	// Source/MeasuredOn — откуда число и когда измерено. Обязательны:
	// цифра рынка без источника и даты через год начнёт врать, и
	// заметить это будет нечем.
	Source     string `json:"source"`
	MeasuredOn string `json:"measured_on"`
	// TimesCheaper — во сколько раз ориентир дороже нашей фактической
	// цены тысячи. Считается на сервере из cost_per_1000: второй расчёт
	// в браузере разошёлся бы с первым.
	TimesCheaper float64 `json:"times_cheaper"`
}

// platformGain — что площадка набрала за окно.
type platformGain struct {
	Views      int64
	Engagement int64
	Comments   int64
	// SharesUnknown — хотя бы один снимок окна пришёл без репостов, и во
	// взаимодействия они не вошли. Признак оконный, а не общий: за всё
	// время площадка могла репосты отдавать, а в этом окне перестать.
	SharesUnknown bool
}

func (g *platformGain) add(o platformGain) {
	g.Views += o.Views
	g.Engagement += o.Engagement
	g.Comments += o.Comments
	g.SharesUnknown = g.SharesUnknown || o.SharesUnknown
}

// dashGains — приросты текущего и предыдущего окна.
type dashGains struct {
	Cur    map[string]platformGain
	Prev   map[string]platformGain
	Series map[string][]OverviewPoint
	// CurTotal/PrevTotal — то же суммой по всем площадкам.
	CurTotal, PrevTotal platformGain
}

// platformTotals — итоги по площадкам за всё время: просмотры, лайки,
// комментарии, репосты и признак «репосты известны не везде».
//
// Один проход по тем же строкам, что и разбивка по проектам: запрос на
// площадку означал бы пять запросов ради пяти чисел.
func (r *Repo) platformTotals(ctx context.Context, clientID uuid.UUID) (map[string]platformStat, error) {
	rows, err := r.db.Query(ctx, `
SELECT platform, SUM(views)::bigint, SUM(likes)::bigint, SUM(comments)::bigint,
       SUM(COALESCE(shares, 0))::bigint, bool_and(shares IS NOT NULL)
FROM (`+clientPlatformStatsSQL+`
) t
GROUP BY platform`, clientID)
	if err != nil {
		return nil, fmt.Errorf("platform totals: %w", err)
	}
	defer rows.Close()
	out := map[string]platformStat{}
	for rows.Next() {
		var (
			platform string
			st       platformStat
		)
		if err := rows.Scan(&platform, &st.Views, &st.Likes, &st.Comments,
			&st.Shares, &st.SharesKnown); err != nil {
			return nil, fmt.Errorf("scan platform totals: %w", err)
		}
		out[platform] = st
	}
	return out, rows.Err()
}

// platformStat — итог одной площадки за всё время.
type platformStat struct {
	Views, Likes, Comments, Shares int64
	// SharesKnown — репосты отдали ВСЕ строки площадки. Одна
	// неизвестная делает приблизительным весь итог: см. sharesTotal в
	// publications, правило то же.
	SharesKnown bool
}

// dashboardGains — приросты по дням и площадкам за оба окна, одним
// запросом.
//
// Ряд поденной статистики накопительный, поэтому прирост считается как
// разница с предыдущим снимком ЭТОЙ ЖЕ ссылки (LAG), а не как разница
// сумм: у ссылок разные дни сбора, и вычитание сумм давало бы провалы
// там, где часть ссылок в этот день просто не обходили.
//
// Предшественник у первого дня окна обязан быть, иначе весь накопленный
// объём засчитается приростом этого дня — заказчик увидит всплеск на
// ровном месте. Раньше ради этого окно LAG считалось по ВСЕЙ истории
// ссылки, а отбор по датам стоял снаружи. Планировщик не может
// протолкнуть предикат внутрь оконной функции, поэтому на двухстах
// проектах через окно прогонялось триста тысяч строк, чтобы отдать
// полторы сотни: 246 мс на один виджет, и растёт это вместе со ВСЕЙ
// накопленной историей, а не с запрошенным окном.
//
// Теперь предшественник берётся адресно: для каждой ссылки — её
// последний снимок строго перед окном, сколько бы дней назад он ни был.
// Собирают не каждый день, поэтому «минус сутки» здесь мало: пропущенный
// день вернул бы ту же неправду, только реже и незаметнее.
func (r *Repo) dashboardGains(ctx context.Context, clientID uuid.UUID, w dashWindow) (dashGains, error) {
	out := dashGains{
		Cur:    map[string]platformGain{},
		Prev:   map[string]platformGain{},
		Series: map[string][]OverviewPoint{},
	}
	rows, err := r.db.Query(ctx, `
WITH mine AS (
    SELECT l.id, l.platform
    FROM projects p
    JOIN project_publications pub ON pub.project_id = p.id AND pub.status <> 'cancelled'
    JOIN publication_links l ON l.publication_id = pub.id
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
), before AS (
    -- Последний снимок каждой ссылки ДО окна. Ровно одна строка на
    -- ссылку: она и служит предшественником первого дня.
    SELECT d.link_id, MAX(d.stat_date) AS prev_date
    FROM video_stat_daily d
    JOIN mine m ON m.id = d.link_id
    WHERE d.stat_date < $2::date
    GROUP BY d.link_id
), daily AS (
    SELECT m.platform, d.stat_date,
           d.views    - COALESCE(LAG(d.views)    OVER w, 0) AS dviews,
           d.likes    - COALESCE(LAG(d.likes)    OVER w, 0) AS dlikes,
           d.comments - COALESCE(LAG(d.comments) OVER w, 0) AS dcomments,
           COALESCE(d.shares, 0) - COALESCE(LAG(COALESCE(d.shares, 0)) OVER w, 0) AS dshares,
           d.shares IS NOT NULL AS has_shares
    FROM video_stat_daily d
    JOIN mine m ON m.id = d.link_id
    LEFT JOIN before b ON b.link_id = d.link_id
    WHERE d.stat_date <= $3::date
      AND d.stat_date >= COALESCE(b.prev_date, $2::date)
    WINDOW w AS (PARTITION BY d.link_id ORDER BY d.stat_date)
)
SELECT platform, stat_date,
       GREATEST(SUM(dviews), 0)::bigint,
       GREATEST(SUM(dlikes + dcomments + dshares), 0)::bigint,
       GREATEST(SUM(dcomments), 0)::bigint,
       bool_and(has_shares)
FROM daily
WHERE stat_date BETWEEN $2::date AND $3::date
GROUP BY platform, stat_date
ORDER BY stat_date`, clientID, w.PrevFrom, w.To)
	if err != nil {
		return out, fmt.Errorf("dashboard gains: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			platform string
			day      time.Time
			g        platformGain
		)
		var haveShares bool
		if err := rows.Scan(&platform, &day, &g.Views, &g.Engagement, &g.Comments,
			&haveShares); err != nil {
			return out, fmt.Errorf("scan dashboard gains: %w", err)
		}
		g.SharesUnknown = !haveShares
		if day.Before(w.From) {
			cur := out.Prev[platform]
			cur.add(g)
			out.Prev[platform] = cur
			out.PrevTotal.add(g)
			continue
		}
		cur := out.Cur[platform]
		cur.add(g)
		out.Cur[platform] = cur
		out.CurTotal.add(g)
		// В ряд идут только дни, по которым сбор был: пропуск честнее
		// нуля — фронт рвёт линию на пропусках, а ноль нарисовал бы
		// провал.
		out.Series[platform] = append(out.Series[platform],
			OverviewPoint{Date: day.Format("2006-01-02"), ViewsGained: g.Views})
	}
	return out, rows.Err()
}

// topVideos — ролики с наибольшим приростом просмотров за окно.
//
// Сортировка и показ по одной величине — по приросту за окно: показать
// «всего просмотров», а отсортировать по приросту значило бы отдать
// список, который выглядит непорядочным.
func (r *Repo) topVideos(ctx context.Context, clientID uuid.UUID, w dashWindow, limit int) ([]OverviewTopVideo, error) {
	rows, err := r.db.Query(ctx, `
WITH mine AS (
    SELECT l.id, l.publication_id, l.platform, l.url
    FROM projects p
    JOIN project_publications pub ON pub.project_id = p.id AND pub.status <> 'cancelled'
    JOIN publication_links l ON l.publication_id = pub.id
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
), daily AS (
    SELECT d.link_id, d.stat_date,
           d.views - COALESCE(LAG(d.views) OVER (PARTITION BY d.link_id ORDER BY d.stat_date), 0) AS dviews
    FROM video_stat_daily d
    JOIN mine m ON m.id = d.link_id
), per_link AS (
    SELECT m.publication_id, m.platform, m.url, GREATEST(SUM(dl.dviews), 0)::bigint AS gain
    FROM daily dl
    JOIN mine m ON m.id = dl.link_id
    WHERE dl.stat_date BETWEEN $2::date AND $3::date
    GROUP BY m.publication_id, m.platform, m.url
), per_pub AS (
    SELECT publication_id, SUM(gain)::bigint AS gain
    FROM per_link GROUP BY publication_id
)
SELECT pp.publication_id, pub.title, lead.platform, lead.url, pp.gain,
       (SELECT MIN(COALESCE(l2.published_at, l2.submitted_at))
        FROM publication_links l2 WHERE l2.publication_id = pp.publication_id)
FROM per_pub pp
JOIN project_publications pub ON pub.id = pp.publication_id
JOIN LATERAL (
    SELECT platform, url FROM per_link pl
    WHERE pl.publication_id = pp.publication_id
    ORDER BY gain DESC, platform LIMIT 1
) lead ON TRUE
WHERE pp.gain > 0
ORDER BY pp.gain DESC, pp.publication_id
LIMIT $4`, clientID, w.From, w.To, limit)
	if err != nil {
		return nil, fmt.Errorf("top videos: %w", err)
	}
	defer rows.Close()
	out := make([]OverviewTopVideo, 0, limit)
	for rows.Next() {
		var (
			v         OverviewTopVideo
			published *time.Time
		)
		if err := rows.Scan(&v.PublicationID, &v.Title, &v.Platform, &v.URL,
			&v.Views, &published); err != nil {
			return nil, fmt.Errorf("scan top video: %w", err)
		}
		if published != nil {
			v.PublishedAt = published.UTC().Format("2006-01-02")
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// lastCollectedAt — когда последний раз собирали статистику по проектам
// заказчика. На дашборде это подпись «данные на такое-то время», и она
// обязана быть правдой: nil, пока не собирали ни разу.
//
// Сбор оставляет ДВЕ отметки, и раньше здесь читалась только одна —
// publication_links.last_collected_at, журнал самого сборщика. Числа
// же, которые видит заказчик, считаются по video_stat_daily, и туда
// снимки попадают не только из сборщика (перелив истории, ручная
// правка, наливка стенда). Получалось «первый сбор ещё не прошёл» под
// тремя миллионами просмотров: цифры собраны, а отметка о сборе
// потеряна по дороге.
//
// Поэтому ответ — самый свежий сбор среди обеих отметок. GREATEST здесь
// именно за этим: он игнорирует NULL и отдаёт NULL, только если пусты
// обе, — то есть «не собирали ни разу» осталось отличимым от «собирали».
func (r *Repo) lastCollectedAt(ctx context.Context, clientID uuid.UUID) (*time.Time, error) {
	var at *time.Time
	if err := r.db.QueryRow(ctx, `
SELECT GREATEST(MAX(l.last_collected_at), MAX(d.collected_at))
FROM publication_links l
JOIN project_publications pub ON pub.id = l.publication_id AND pub.status <> 'cancelled'
JOIN projects p ON p.id = pub.project_id
LEFT JOIN video_stat_daily d ON d.link_id = l.id
WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'`,
		clientID).Scan(&at); err != nil {
		return nil, fmt.Errorf("last collected at: %w", err)
	}
	return at, nil
}

// fillDashboard — блоки дашборда поверх уже посчитанной сводки.
//
// Итоги берутся из тех же строк, что и остальная сводка, приросты — из
// одного запроса на оба окна.
func (s *Service) fillDashboard(ctx context.Context, out *ClientOverview, clientID uuid.UUID, w dashWindow) error {
	out.Range = w.Name
	out.RangeLabel = w.Label

	totals, err := s.repo.platformTotals(ctx, clientID)
	if err != nil {
		return err
	}
	gains, err := s.repo.dashboardGains(ctx, clientID, w)
	if err != nil {
		return err
	}
	if out.TopVideos, err = s.repo.topVideos(ctx, clientID, w, topVideosLimit); err != nil {
		return err
	}
	if out.CollectedAt, err = s.repo.lastCollectedAt(ctx, clientID); err != nil {
		return err
	}

	// Итоги по всем площадкам сразу: вовлечённость и её звёздочка
	// считаются от одних и тех же сумм.
	var all platformStat
	all.SharesKnown = true
	for _, p := range publications.AllPlatforms {
		st := totals[p]
		all.Views += st.Views
		all.Likes += st.Likes
		all.Comments += st.Comments
		if st.SharesKnown {
			all.Shares += st.Shares
			continue
		}
		// Площадка без роликов репостов не теряет: терять нечего.
		if st.Views > 0 || st.Likes > 0 || st.Comments > 0 {
			all.SharesKnown = false
		}
	}

	// Итоги за всё время — без приростов: прирост при них описывал бы
	// другую величину, чем само число.
	out.Engagement = OverviewEngagement{
		Total:    all.Likes + all.Comments + all.Shares,
		Comments: all.Comments,
	}
	out.ER = OverviewER{WithoutShares: !all.SharesKnown}
	if er := erPercent(all.Likes+all.Comments+all.Shares, all.Views); er != nil {
		out.ER.Percent = *er
	}

	// Окно — своими именами, и прирост стоит рядом с той величиной,
	// которую описывает.
	out.Window = OverviewWindow{
		Views:              gains.CurTotal.Views,
		ViewsDeltaPct:      deltaPct(gains.CurTotal.Views, gains.PrevTotal.Views),
		Engagement:         gains.CurTotal.Engagement,
		EngagementDeltaPct: deltaPct(gains.CurTotal.Engagement, gains.PrevTotal.Engagement),
		Comments:           gains.CurTotal.Comments,
		CommentsDeltaPct:   deltaPct(gains.CurTotal.Comments, gains.PrevTotal.Comments),
		ERWithoutShares:    gains.CurTotal.SharesUnknown,
	}
	curER, prevER := erPercent(gains.CurTotal.Engagement, gains.CurTotal.Views),
		erPercent(gains.PrevTotal.Engagement, gains.PrevTotal.Views)
	if curER != nil {
		out.Window.ERPercent = *curER
	}
	if curER != nil && prevER != nil {
		d := math.Round((*curER-*prevER)*10) / 10
		out.Window.ERDeltaPP = &d
	}

	out.Platforms = make([]OverviewPlatform, 0, len(publications.AllPlatforms))
	for _, p := range publications.AllPlatforms {
		st := totals[p]
		row := OverviewPlatform{
			Platform:        p,
			Views:           st.Views,
			WindowViews:     gains.Cur[p].Views,
			ERWithoutShares: !st.SharesKnown && (st.Views > 0 || st.Likes > 0 || st.Comments > 0),
			DeltaPct:        deltaPct(gains.Cur[p].Views, gains.Prev[p].Views),
			Series:          gains.Series[p],
		}
		if row.Series == nil {
			row.Series = []OverviewPoint{}
		}
		engagement := st.Likes + st.Comments
		if st.SharesKnown {
			engagement += st.Shares
		}
		row.ERPercent = erPercent(engagement, st.Views)
		if all.Views > 0 {
			row.SharePct = int(math.Round(float64(st.Views) / float64(all.Views) * 100))
		}
		// Оконная доля считается от ОКОННОЙ суммы: полоса состава стоит
		// рядом с главным числом дашборда и обязана сходиться с ним.
		if gains.CurTotal.Views > 0 {
			row.WindowSharePct = int(math.Round(
				float64(gains.Cur[p].Views) / float64(gains.CurTotal.Views) * 100))
		}
		out.Platforms = append(out.Platforms, row)
	}
	// Площадки — по убыванию просмотров, пустые в конце. Все пять
	// всегда: пропавший столбик читается как сбой, а не как ноль.
	sortPlatforms(out.Platforms)
	return nil
}

// erPercent — вовлечённость в процентах. nil, если просмотров нет:
// делить не на что, а ноль читался бы как «никто не реагирует».
func erPercent(engagement, views int64) *float64 {
	if views <= 0 {
		return nil
	}
	v := math.Round(float64(engagement)/float64(views)*1000) / 10
	return &v
}

func sortPlatforms(items []OverviewPlatform) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			if items[j].Views > items[j-1].Views ||
				(items[j].Views == items[j-1].Views && items[j].Platform < items[j-1].Platform) {
				items[j], items[j-1] = items[j-1], items[j]
				continue
			}
			break
		}
	}
}

// fillMarket — сравнение с рынком из справочника порогов.
//
// Блока нет вовсе, пока нет своей цены тысячи: «дешевле в бесконечность
// раз» — не аргумент, а деление на ноль. Источник и дата замера идут
// рядом с каждым числом обязательно: цифра рынка протухает, и без них
// заметить это будет нечем.
func (s *Service) fillMarket(ctx context.Context, out *ClientOverview) error {
	if out.CostPer1000 == nil || *out.CostPer1000 <= 0 {
		return nil
	}
	scale, err := s.scales.Current(ctx)
	if err != nil {
		// Справочник ещё не выпущен — блока просто нет. Это не повод
		// ронять весь экран.
		return nil
	}
	ours := float64(*out.CostPer1000)
	items := make([]OverviewMarket, 0, len(scale.Market))
	for _, m := range scale.Market {
		if m.PricePer1000 <= 0 {
			continue
		}
		items = append(items, OverviewMarket{
			Key:          m.Key,
			Title:        m.Title,
			PricePer1000: m.PricePer1000,
			Source:       m.Source,
			MeasuredOn:   m.MeasuredOn.UTC().Format("2006-01-02"),
			TimesCheaper: math.Round(float64(m.PricePer1000)/ours*10) / 10,
		})
	}
	if len(items) == 0 {
		return nil
	}
	out.Market = items
	out.MarketScaleVersion = scale.Version
	return nil
}
