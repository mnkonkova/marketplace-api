package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
	"marketpclce/internal/publications"
)

// Сводка заказчика по всем его проектам.
//
// Такого среза в продукте не было вовсе: всё, что есть, считается внутри
// одного проекта, и заказчик с тремя проектами складывал числа в уме или
// в табличке. Отсюда и главный вопрос этого экрана — «сколько мне стоит
// тысяча просмотров» — на который в проекте по отдельности ответа нет.
//
// Деньги здесь только клиентской стороны: типы свои, менеджерских
// структур не встраивают. Новое поле про выплаты креаторам или маржу не
// попадёт сюда само — его придётся положить руками, а этого никто не
// сделает.

// overviewProjectsLimit — сколько проектов берём в сводку.
//
// Экран открывается на каждый заход, а у заказчика проектов может быть
// много. Сто — заведомо больше любого реального числа; упрёмся —
// добавим страницу, а пока молча отдавать всё было бы обещанием, которое
// однажды не выполнится.
const overviewProjectsLimit = 100

// overviewSeriesDays — глубина ряда для графика. Три месяца: на графике
// виден рост, а не история проекта целиком.
const overviewSeriesDays = 90

// Состояния проекта в сводке.
const (
	// OverviewRunning — проект идёт: есть вышедшие ролики и период.
	OverviewRunning = "running"
	// OverviewNotStarted — ещё не начался: ни одного вышедшего ролика, а
	// значит и периода. Нули у такого проекта означали бы «работаем и
	// ничего не набрали», а это другое.
	OverviewNotStarted = "not_started"
)

// OverviewViews — просмотры: всего и по площадкам.
//
// Именно просмотры, не охват: охват площадки отдают отдельно и не везде,
// и назвать одно другим значило бы пообещать то, чего мы не собираем.
type OverviewViews struct {
	Total int64 `json:"total"`
	// ByPlatform — все пять площадок всегда, включая нулевые: пропавший
	// столбик читается как сбой, а не как ноль.
	ByPlatform map[string]int64 `json:"by_platform"`
}

// OverviewMoney — деньги заказчика по всем проектам, в копейках.
type OverviewMoney struct {
	// Locked — сумма по подытоженным периодам: окончательная, из срезов.
	Locked int64 `json:"locked"`
	// Current — по текущим периодам: предварительная, числа ещё вырастут.
	Current int64 `json:"current"`
	// Total — Locked + Current, «сколько всего стоит работа на сегодня».
	Total int64 `json:"total"`
	// Paid — сколько заказчик уже перевёл: подтверждённые платежи.
	// Отдельно от Total намеренно — это разные вопросы: «сколько стоит»
	// и «сколько внесено».
	Paid int64 `json:"paid"`
}

// OverviewProject — строка разбивки по проектам.
type OverviewProject struct {
	ProjectID uuid.UUID `json:"project_id"`
	Title     string    `json:"title"`
	// State — running | not_started.
	State string `json:"state"`
	// Period — текущий период проекта. nil у не начавшегося.
	Period *ClientPeriod `json:"period,omitempty"`
	Views  int64         `json:"views"`
	// Total — счёт по проекту: подытоженные периоды плюс текущий.
	Total int64 `json:"total"`
	// CostPer1000 — стоимость тысячи просмотров по этому проекту. nil,
	// пока просмотров нет: делить не на что.
	CostPer1000 *int64 `json:"cost_per_1000,omitempty"`
}

// OverviewPoint — точка графика.
type OverviewPoint struct {
	Date string `json:"date"`
	// ViewsGained — сколько просмотров набрали за этот день, а не всего
	// на эту дату. Поденный ряд накопительный, и «всего на дату»
	// пришлось бы достраивать по каждой ссылке за каждый день; прирост
	// показывает ровно то, ради чего график и смотрят.
	ViewsGained int64 `json:"views_gained"`
}

// ClientOverview — ответ сводного экрана.
type ClientOverview struct {
	ProjectsTotal int               `json:"projects_total"`
	Projects      []OverviewProject `json:"projects"`
	Views         OverviewViews     `json:"views"`
	Money         OverviewMoney     `json:"money"`
	// CostPer1000 — стоимость тысячи просмотров по всем проектам, в
	// копейках. Считается из ФАКТИЧЕСКИХ сумм и фактических просмотров,
	// а не из ставки тарифа: у разных проектов разные версии условий, и
	// средняя ставка соврала бы. nil, пока просмотров нет.
	CostPer1000 *int64 `json:"cost_per_1000,omitempty"`
	// SnapshotApprox — хотя бы у одного подытоженного периода числа
	// подтянуты: поденную статистику к моменту подытога уже удалили.
	// Сводка, часть чисел которой приблизительна, обязана сказать об
	// этом, а не выглядеть точной.
	SnapshotApprox bool `json:"snapshot_approx,omitempty"`
	// Series — ряд для графика, последние 90 дней.
	Series      []OverviewPoint `json:"series"`
	GeneratedAt time.Time       `json:"generated_at"`
}

// costPer1000 — во сколько обходится тысяча просмотров. nil, если
// просмотров нет: ноль в знаменателе — это не «бесплатно», это «нечего
// делить».
func costPer1000(total, views int64) *int64 {
	if views <= 0 {
		return nil
	}
	v := total * 1000 / views
	return &v
}

// overviewProject — проект заказчика в сводке (до подсчётов).
type overviewProject struct {
	ID    uuid.UUID
	Title string
}

// ClientProjects — проекты заказчика для сводки. Тестовые не в счёт:
// они есть у каждого, кто щупал стенд, и в его же сводке им не место.
func (r *Repo) ClientProjects(ctx context.Context, clientID uuid.UUID, limit int) ([]overviewProject, error) {
	if limit <= 0 {
		limit = overviewProjectsLimit
	}
	rows, err := r.db.Query(ctx, `
SELECT id, title
FROM projects
WHERE client_user_id = $1 AND is_test = FALSE AND status <> 'cancelled'
ORDER BY created_at
LIMIT $2`, clientID, limit)
	if err != nil {
		return nil, fmt.Errorf("client projects: %w", err)
	}
	defer rows.Close()
	out := make([]overviewProject, 0, 8)
	for rows.Next() {
		var p overviewProject
		if err := rows.Scan(&p.ID, &p.Title); err != nil {
			return nil, fmt.Errorf("scan client project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// platformViews — просмотры по (проект, площадка) одним запросом на всю
// сводку, а не по запросу на проект.
//
// Две половины, потому что источник разный и это принципиально:
// подытоженный период отдаёт числа из среза (они заморожены), всё
// остальное — живые, по последнему снимку каждой ссылки. Ролик попадает
// ровно в одну половину: выкладка принадлежит одному периоду.
func (r *Repo) platformViews(ctx context.Context, clientID uuid.UUID) (map[uuid.UUID]map[string]int64, error) {
	rows, err := r.db.Query(ctx, `
SELECT project_id, platform, SUM(views)::bigint FROM (
    -- Подытоженные периоды: числа из среза.
    SELECT pp.project_id, v.platform, COALESCE(v.views, 0) AS views
    FROM project_period_views v
    JOIN project_periods pp ON pp.id = v.period_id AND pp.status = 'locked'
    JOIN projects p ON p.id = pp.project_id
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'

    UNION ALL

    -- Всё, что ещё не подытожено: живые числа.
    SELECT p.id, l.platform, COALESCE(cur.views, 0) AS views
    FROM projects p
    JOIN project_publications pub ON pub.project_id = p.id AND pub.status <> 'cancelled'
    JOIN publication_links l ON l.publication_id = pub.id
    LEFT JOIN LATERAL (
        SELECT views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
      AND NOT EXISTS (
          SELECT 1 FROM project_period_publications spp
          JOIN project_periods pp2 ON pp2.id = spp.period_id AND pp2.status = 'locked'
          WHERE spp.publication_id = pub.id
      )
) t
GROUP BY project_id, platform`, clientID)
	if err != nil {
		return nil, fmt.Errorf("platform views: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]map[string]int64{}
	for rows.Next() {
		var (
			pid      uuid.UUID
			platform string
			views    int64
		)
		if err := rows.Scan(&pid, &platform, &views); err != nil {
			return nil, fmt.Errorf("scan platform views: %w", err)
		}
		if out[pid] == nil {
			out[pid] = map[string]int64{}
		}
		out[pid][platform] += views
	}
	return out, rows.Err()
}

// paidByClient — сколько заказчик уже перевёл: подтверждённые платежи по
// всем его проектам.
func (r *Repo) paidByClient(ctx context.Context, clientID uuid.UUID) (int64, error) {
	var total int64
	if err := r.db.QueryRow(ctx, `
SELECT COALESCE(SUM(pay.amount), 0)
FROM project_payments pay
JOIN projects p ON p.id = pay.project_id
WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
  AND pay.status = 'confirmed'`, clientID).Scan(&total); err != nil {
		return 0, fmt.Errorf("paid by client: %w", err)
	}
	return total, nil
}

// lockedTotals — счёт по подытоженным периодам, одним запросом на всю
// сводку. Берём сохранённые строки начислений: у подытоженного периода
// они есть всегда — подытог их и пересчитывает.
//
// Возвращает сумму по проектам и признак «хоть где-то числа подтянуты».
func (r *Repo) lockedTotals(ctx context.Context, clientID uuid.UUID) (map[uuid.UUID]int64, bool, error) {
	rows, err := r.db.Query(ctx, `
SELECT a.project_id, COALESCE(SUM(a.total), 0)::bigint,
       bool_or(pp.snapshot_approx)
FROM creator_accruals a
JOIN project_periods pp
  ON pp.project_id = a.project_id AND pp.starts_on = a.period_start AND pp.status = 'locked'
JOIN projects p ON p.id = a.project_id
WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
GROUP BY a.project_id`, clientID)
	if err != nil {
		return nil, false, fmt.Errorf("locked totals: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	approx := false
	for rows.Next() {
		var (
			pid       uuid.UUID
			total     int64
			hasApprox *bool
		)
		if err := rows.Scan(&pid, &total, &hasApprox); err != nil {
			return nil, false, fmt.Errorf("scan locked totals: %w", err)
		}
		out[pid] = total
		if hasApprox != nil && *hasApprox {
			approx = true
		}
	}
	return out, approx, rows.Err()
}

// viewsSeries — прирост просмотров по дням за последние days дней, по
// всем проектам заказчика. Один запрос на весь график.
//
// Ряд накопительный по каждой ссылке, поэтому прирост считается как
// разница с предыдущим снимком ЭТОЙ ЖЕ ссылки (LAG), а не как разница
// сумм: у ссылок разные дни сбора, и вычитание сумм давало бы провалы
// там, где часть ссылок в этот день просто не обходили.
func (r *Repo) viewsSeries(ctx context.Context, clientID uuid.UUID, from time.Time) ([]OverviewPoint, error) {
	rows, err := r.db.Query(ctx, `
WITH mine AS (
    SELECT l.id
    FROM projects p
    JOIN project_publications pub ON pub.project_id = p.id AND pub.status <> 'cancelled'
    JOIN publication_links l ON l.publication_id = pub.id
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
), daily AS (
    SELECT d.link_id, d.stat_date, d.views,
           LAG(d.views) OVER (PARTITION BY d.link_id ORDER BY d.stat_date) AS prev
    FROM video_stat_daily d
    JOIN mine m ON m.id = d.link_id
)
SELECT stat_date, GREATEST(SUM(views - COALESCE(prev, 0)), 0)::bigint
FROM daily
WHERE stat_date >= $2::date
GROUP BY stat_date
ORDER BY stat_date`, clientID, from)
	if err != nil {
		return nil, fmt.Errorf("views series: %w", err)
	}
	defer rows.Close()
	out := make([]OverviewPoint, 0, 32)
	for rows.Next() {
		var (
			day   time.Time
			delta int64
		)
		if err := rows.Scan(&day, &delta); err != nil {
			return nil, fmt.Errorf("scan series point: %w", err)
		}
		out = append(out, OverviewPoint{Date: day.Format("2006-01-02"), ViewsGained: delta})
	}
	return out, rows.Err()
}

// ---- сервис ----

// ClientOverview — сводка по всем проектам заказчика.
//
// Срезы считаются пачкой: просмотры, подытоженные суммы, платежи и
// график — по одному запросу на каждый, а не по запросу на проект.
// Текущий период — исключение: его сумма считается тем же кодом, что и
// на странице проекта (accrualsOrPreview), и другого способа не
// разойтись с ней в числах нет. Переписать ту же арифметику ещё раз,
// пачкой, значило бы завести третью копию денежных правил — за это мы
// уже платили.
func (s *Service) ClientOverview(ctx context.Context, clientID uuid.UUID, now time.Time) (ClientOverview, error) {
	out := ClientOverview{
		Projects:    []OverviewProject{},
		Series:      []OverviewPoint{},
		GeneratedAt: now,
		Views:       OverviewViews{ByPlatform: map[string]int64{}},
	}
	// Все пять площадок всегда: пропавший столбик читается как сбой.
	for _, p := range publications.AllPlatforms {
		out.Views.ByPlatform[p] = 0
	}

	projects, err := s.repo.ClientProjects(ctx, clientID, overviewProjectsLimit)
	if err != nil {
		return out, err
	}
	out.ProjectsTotal = len(projects)
	if len(projects) == 0 {
		return out, nil
	}

	viewsByProject, err := s.repo.platformViews(ctx, clientID)
	if err != nil {
		return out, err
	}
	lockedByProject, approx, err := s.repo.lockedTotals(ctx, clientID)
	if err != nil {
		return out, err
	}
	out.SnapshotApprox = approx
	if out.Money.Paid, err = s.repo.paidByClient(ctx, clientID); err != nil {
		return out, err
	}
	if out.Series, err = s.repo.viewsSeries(ctx, clientID, now.AddDate(0, 0, -overviewSeriesDays)); err != nil {
		return out, err
	}

	for _, pr := range projects {
		row := OverviewProject{
			ProjectID: pr.ID,
			Title:     pr.Title,
			State:     OverviewNotStarted,
		}
		for platform, v := range viewsByProject[pr.ID] {
			row.Views += v
			// Площадка вне пятёрки в сводку не идёт: ключи фиксированы,
			// а чужая площадка означала бы битую ссылку в базе.
			if _, ok := out.Views.ByPlatform[platform]; ok {
				out.Views.ByPlatform[platform] += v
			}
		}
		out.Views.Total += row.Views

		locked := lockedByProject[pr.ID]
		row.Total = locked
		out.Money.Locked += locked

		// Текущий период — если он вообще есть.
		period, perr := s.Period(ctx, pr.ID, 0, now)
		switch {
		case errors.Is(perr, ErrNoPeriods):
			// Проект в сводке есть, но помечен «ещё не начался»: нулей,
			// неотличимых от «работаем и ничего не набрали», быть не должно.
		case perr != nil:
			return out, perr
		default:
			row.State = OverviewRunning
			cp := clientPeriodView(period)
			row.Period = &cp
			if period.SnapshotApprox {
				out.SnapshotApprox = true
			}
			if !period.IsLocked() {
				terms, terr := s.repo.Terms(ctx, pr.ID)
				if terr != nil {
					return out, terr
				}
				accruals, aerr := s.accrualsOrPreview(ctx, pr.ID, period, terms)
				if aerr != nil {
					return out, aerr
				}
				current := totals(accruals).Total
				row.Total += current
				out.Money.Current += current
			}
		}

		row.CostPer1000 = costPer1000(row.Total, row.Views)
		out.Projects = append(out.Projects, row)
	}

	out.Money.Total = out.Money.Locked + out.Money.Current
	// Стоимость тысячи — из фактических сумм и фактических просмотров.
	// Средняя ставка по тарифам соврала бы: у проектов разные версии
	// условий, и цена тысячи у них разная.
	out.CostPer1000 = costPer1000(out.Money.Total, out.Views.Total)
	return out, nil
}

// ClientOverviewHandler godoc
// @Summary  Сводка по всем моим проектам (заказчик)
// @Description Кросс-проектный срез: просмотры по пяти площадкам и всего,
// @Description счёт по подытоженным периодам и по текущим, сколько уже
// @Description переведено, стоимость тысячи просмотров и разбивка по
// @Description проектам. Стоимость считается из фактических сумм и
// @Description просмотров, а не из ставки тарифа: у проектов бывают
// @Description разные версии условий.
// @Description Подытоженные периоды дают числа из среза, текущие — живые.
// @Description snapshot_approx означает, что часть чисел подтянута:
// @Description поденную статистику к моменту подытога уже удалили.
// @Description Тестовые проекты не учитываются.
// @Tags     client-billing
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} ClientOverview
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/overview [get]
func (h *Handler) ClientOverviewHandler(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	out, err := h.svc.ClientOverview(r.Context(), uid, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ProjectBenchmark — обезличенный ориентир проекта для кабинета
// креатора: медиана просмотров зрелых роликов и место человека
// относительно неё.
//
// Зрелым считается ролик старше двух недель — тех же, что у отсечки
// периода: пока он растёт, сравнивать его с отлежавшимися нечестно.
// Возраст берём от фактической даты публикации, а где её нет — от сдачи
// ссылки, как и везде.
//
// Порог обезличивания проверяется ЗДЕСЬ, а не в вызывающем: иначе рано
// или поздно появится второй вызывающий, который про порог не знает.
// Не прошли порог — возвращаем nil, и поля в ответе не будет.
func (r *Repo) ProjectBenchmark(ctx context.Context, projectID, creatorID uuid.UUID, now time.Time) (*ProjectBenchmark, error) {
	cutoff := now.Add(-matureVideoAge)

	// Одна выкладка — один ролик: просмотры суммируются по площадкам, и
	// считать ролик пять раз значило бы завысить и медиану, и порог.
	const q = `
WITH mature AS (
    SELECT p.id, p.creator_user_id,
           COALESCE(SUM(cur.views), 0) AS views
    FROM project_publications p
    JOIN publication_links l ON l.publication_id = p.id
    LEFT JOIN LATERAL (
        SELECT views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE p.project_id = $1 AND p.status <> 'cancelled'
    GROUP BY p.id, p.creator_user_id
    HAVING MIN(COALESCE(l.published_at, l.submitted_at)) <= $3
)
SELECT
    COUNT(*)::int,
    COUNT(DISTINCT creator_user_id)::int,
    COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY views), 0)::bigint,
    -- Медиана самого спрашивающего: с ней и сравниваем. NULL, если
    -- зрелых роликов у него нет.
    (SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY views)
     FROM mature WHERE creator_user_id = $2),
    -- Доля роликов проекта, которые слабее его медианного.
    (SELECT COUNT(*)::int FROM mature
     WHERE views < (SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY views)
                    FROM mature WHERE creator_user_id = $2))
FROM mature`

	var (
		videos, creators, weaker int
		median                   int64
		mine                     *float64
	)
	if err := r.db.QueryRow(ctx, q, projectID, creatorID, cutoff).
		Scan(&videos, &creators, &median, &mine, &weaker); err != nil {
		return nil, fmt.Errorf("project benchmark: %w", err)
	}
	// Порог обезличивания: мало роликов или мало людей — это уже не
	// агрегат, а чужой показатель. Отдаём пусто.
	if videos < medianMinVideos || creators < medianMinCreators {
		return nil, nil
	}
	out := &ProjectBenchmark{MedianViews: median, MatureVideos: videos}
	if mine != nil && videos > 0 {
		p := weaker * 100 / videos
		out.MyPercentile = &p
	}
	return out, nil
}
