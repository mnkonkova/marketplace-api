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
	"marketpclce/internal/projects"
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
	// OverviewCompleted — проект закончен (projects.status = 'done').
	//
	// Третье состояние, а не «running с флажком»: по нему заказчик
	// отбирает строку в блок итогов, и вычислять завершённость по
	// косвенным признакам — вроде «период закрыт и новый не начался» —
	// значит завести в браузере вторую, расходящуюся версию правды.
	// Состояние проекта знает сервер.
	OverviewCompleted = "completed"
)

// OverviewViews — просмотры: всего и по площадкам.
//
// Именно просмотры, не охват: охват площадки отдают отдельно и не везде,
// и назвать одно другим значило бы пообещать то, чего мы не собираем.
type OverviewViews struct {
	// Total — за всё время по всем проектам заказчика. Не за окно
	// дашборда: по этому же числу считается стоимость тысячи, и
	// подменить его оконным значило бы тихо поменять смысл поля.
	// Оконные просмотры лежат отдельно, в блоке window.
	Total int64 `json:"total"`
	// ByPlatform — все пять площадок всегда, включая нулевые: пропавший
	// столбик читается как сбой, а не как ноль.
	ByPlatform map[string]int64 `json:"by_platform"`
}

// OverviewMoney — деньги заказчика по всем проектам, в копейках.
type OverviewMoney struct {
	// Locked — сумма по подытоженным периодам: окончательная, из срезов.
	Locked int64 `json:"locked"`
	// Current — по НЕПОДЫТОЖЕННЫМ периодам: идущему и тем, что уже
	// кончились, но ждут отсечки. Предварительная: числа ещё вырастут.
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
	// State — running | not_started | completed.
	State string `json:"state"`
	// CompletedAt — когда проект закончили. Есть только у завершённого:
	// итог без даты нечем привязать к «что мы сделали в прошлом году».
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Period — текущий период проекта. nil у не начавшегося.
	Period *ClientPeriod `json:"period,omitempty"`
	Views  int64         `json:"views"`
	// Total — счёт по проекту: подытоженные периоды плюс все ещё не
	// подытоженные, включая кончившийся и ждущий отсечки.
	Total int64 `json:"total"`
	// CostPer1000 — стоимость тысячи просмотров по этому проекту. nil,
	// пока просмотров нет: делить не на что.
	CostPer1000 *int64 `json:"cost_per_1000,omitempty"`
}

// OverviewTariff — почему тысяча стоит столько, сколько показано.
//
// На экране у заказчика стояло ТОЛЬКО «56 ₽ за тысячу» — результат
// формулы без самой формулы. А коммерческий аргумент здесь именно в
// формуле: первые просмотры каждого ролика идут по стартовой ставке,
// всё, что ролик набрал сверх, — по пониженной, и чем дальше он
// расходится, тем ниже выходит средняя цена тысячи. Без разложения это
// читается как «повезло», хотя так устроен договор.
//
// Порог — НА РОЛИК, суммой по пяти площадкам (см. Terms.
// BonusViewsThreshold и LEAST(views, порог) в periodFacts). Написать
// «первый миллион просмотров проекта» значило бы описать чужое правило:
// у пяти роликов по 400 000 сверхпорогового объёма нет вовсе.
//
// Блока нет вовсе, если разложение не сойдётся с показанным итогом —
// см. tariffLadder.result. Лесенка, которая не делится в стоящую рядом
// цену тысячи, хуже отсутствующей: её проверяют на калькуляторе первым
// же заходом.
type OverviewTariff struct {
	// ThresholdViews — сколько просмотров КАЖДОГО ролика идёт по
	// стартовой ставке.
	ThresholdViews int64 `json:"threshold_views"`
	// RatePer1000 / RatePer1000Over — ставки за тысячу до порога и
	// сверх него, копейки.
	RatePer1000     int64 `json:"rate_per_1000"`
	RatePer1000Over int64 `json:"rate_per_1000_over"`
	// ViewsBase / ViewsOver — сколько просмотров легло на каждую
	// ступень, и BaseAmount / OverAmount — во сколько это обошлось.
	ViewsBase  int64 `json:"views_base"`
	ViewsOver  int64 `json:"views_over"`
	BaseAmount int64 `json:"base_amount"`
	OverAmount int64 `json:"over_amount"`
	// Fixed — работа команды: оклады за вычетом недосдачи. Третья
	// строка лесенки, и без неё сумма не сходится с итогом.
	Fixed int64 `json:"fixed"`
	// Views / Total — что разложено: ровно те просмотры и ровно та
	// сумма, из которых посчитана цена тысячи рядом.
	Views int64 `json:"views"`
	Total int64 `json:"total"`
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
	// Tariff — разложение этой цены по ступеням тарифа. nil, когда
	// разложение не сошлось бы с итогом: см. OverviewTariff и tariffLadder.
	Tariff *OverviewTariff `json:"tariff,omitempty"`
	// SnapshotApprox — хотя бы у одного подытоженного периода числа
	// подтянуты: поденную статистику к моменту подытога уже удалили.
	// Сводка, часть чисел которой приблизительна, обязана сказать об
	// этом, а не выглядеть точной.
	SnapshotApprox bool `json:"snapshot_approx,omitempty"`
	// Series — ряд для графика, последние 90 дней.
	Series      []OverviewPoint `json:"series"`
	GeneratedAt time.Time       `json:"generated_at"`

	// ---- дашборд ----
	//
	// Окно скользящее, от сегодня назад: week=7, month=30, quarter=90
	// дней. Итоги в блоках выше — за всё время, приросты — окно против
	// предыдущего окна такой же длины (см. dashboard.go).

	// Range — какое окно посчитано: week | month | quarter.
	Range string `json:"range" enums:"week,month,quarter"`
	// RangeLabel — подпись окна человеку: «17 авг. — 15 сент. 2026».
	// Считается на сервере, чтобы в браузере не завелась вторая
	// реализация русских сокращений месяцев.
	RangeLabel string `json:"range_label"`
	// Window — числа ЗА ОКНО: главный заголовок дашборда берётся отсюда.
	// Приросты живут только здесь, рядом с величинами, которые они
	// описывают.
	Window     OverviewWindow     `json:"window"`
	Engagement OverviewEngagement `json:"engagement"`
	ER         OverviewER         `json:"er"`
	// Platforms — все пять площадок всегда, по убыванию просмотров.
	Platforms []OverviewPlatform `json:"platforms"`
	// TopVideos — три ролика с наибольшим приростом просмотров за окно.
	TopVideos []OverviewTopVideo `json:"top_videos"`
	// CollectedAt — когда последний раз собирали статистику по проектам
	// этого заказчика. Подпись «данные на такое-то время» обязана быть
	// правдой, поэтому nil, пока не собирали ни разу.
	CollectedAt *time.Time `json:"collected_at,omitempty"`
	// Market — во сколько раз рынок дороже нас. Блока нет вовсе, пока
	// нет своей цены тысячи: делить не на что.
	Market []OverviewMarket `json:"market,omitempty"`
	// MarketScaleVersion — версия справочника порогов, из которой взяты
	// ориентиры. Чтобы через полгода на вопрос «откуда цифры» был один и
	// тот же ответ.
	MarketScaleVersion int `json:"market_scale_version,omitempty"`
}

// tariffLadder — разложение счёта по ступеням, копится по ходу сводки.
//
// Складывает ровно то и ровно так, как это посчитал расчёт начислений:
// ступени разложены по роликам ещё в SQL, а деление на тысячу целое и
// идёт по КАЖДОЙ строке креатора отдельно (см. accrue). Сложить
// просмотры и поделить один раз в конце — значит получить другое число,
// и лесенка перестала бы сходиться со счётом на рубли.
type tariffLadder struct {
	// mixed — у проектов разные ставки или порог. Одной лесенкой такое
	// не описать: строка «первый миллион по 90 ₽» была бы неправдой для
	// половины проектов, а сложить две лесенки в одну нельзя — читатель
	// решит, что порог общий.
	mixed bool
	set   bool

	threshold, rate, rateOver     int64
	viewsBase, viewsOver          int64
	baseAmount, overAmount, fixed int64
	total                         int64
}

func (l *tariffLadder) add(t Terms, accruals []Accrual) {
	// Ступенчатая версия условий считается по другим правилам — там нет
	// ни оклада, ни ставки за тысячу (см. Terms.Stepped). Показать её
	// этой лесенкой значит показать поля, которые в расчёте не участвуют.
	if t.Stepped() || t.BonusViewsThreshold <= 0 {
		l.mixed = true
		return
	}
	if !l.set {
		l.set = true
		l.threshold, l.rate, l.rateOver = t.BonusViewsThreshold, t.RatePer1000Views, t.RatePer1000ViewsOver
	} else if l.threshold != t.BonusViewsThreshold || l.rate != t.RatePer1000Views ||
		l.rateOver != t.RatePer1000ViewsOver {
		l.mixed = true
		return
	}
	for _, a := range accruals {
		l.viewsBase += a.ViewsBase
		l.viewsOver += a.ViewsOver
		l.baseAmount += a.ViewsBase / 1000 * l.rate
		l.overAmount += a.ViewsOver / 1000 * l.rateOver
		l.fixed += a.Salary - a.Deduction
		l.total += a.Total
	}
}

// result — лесенка, если она сходится с тем, что показано рядом.
//
// Два условия, и оба про арифметику на экране. Разложение обязано
// покрывать ВЕСЬ счёт (подытоженные периоды сюда не попадают: их
// начислений уже нет, есть только сумма среза) и ВСЕ просмотры, из
// которых посчитана цена тысячи. Иначе рядом встанут «168 000 ₽ ÷ 3 000
// тысяч = 56 ₽» и лесенка на другие деньги — а её проверяют
// калькулятором первым же заходом.
//
// Третье, неявное: сумма ступеней должна совпасть с итогом. Не совпадёт
// она при бонусах, которых в лесенке нет вовсе (переходы, подписчики), —
// и тогда лесенка молча исчезнет, а не покажет счёт, в котором не
// хватает строки.
func (l *tariffLadder) result(money, views int64) *OverviewTariff {
	if !l.set || l.mixed || views <= 0 || money <= 0 {
		return nil
	}
	if l.total != money || l.viewsBase+l.viewsOver != views {
		return nil
	}
	if l.baseAmount+l.overAmount+l.fixed != l.total {
		return nil
	}
	return &OverviewTariff{
		ThresholdViews:  l.threshold,
		RatePer1000:     l.rate,
		RatePer1000Over: l.rateOver,
		ViewsBase:       l.viewsBase,
		ViewsOver:       l.viewsOver,
		BaseAmount:      l.baseAmount,
		OverAmount:      l.overAmount,
		Fixed:           l.fixed,
		Views:           l.viewsBase + l.viewsOver,
		Total:           l.total,
	}
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
	// Kind — вид проекта. Нужен ровно ради цены просмотра: у проекта
	// без креаторов она считается не из начислений (их не бывает), а
	// из суммы, которую назвал менеджер.
	Kind string
	// Done/CompletedAt — проект закончен. Берём из projects.status, а не
	// выводим из периодов: закрытый период и законченный проект — разные
	// вещи, и второе знает только сам проект.
	Done        bool
	CompletedAt *time.Time
}

// ClientProjects — проекты заказчика для сводки. Тестовые не в счёт:
// они есть у каждого, кто щупал стенд, и в его же сводке им не место.
func (r *Repo) ClientProjects(ctx context.Context, clientID uuid.UUID, limit int) ([]overviewProject, error) {
	if limit <= 0 {
		limit = overviewProjectsLimit
	}
	rows, err := r.db.Query(ctx, `
SELECT id, title, kind::text, status = 'done', completed_at
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
		if err := rows.Scan(&p.ID, &p.Title, &p.Kind, &p.Done, &p.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan client project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// projectCosts — названные менеджером стоимости проектов заказчика.
//
// Одним запросом на всю сводку, а не по запросу на проект: проектов у
// заказчика бывает восемь, и ради одного числа ходить в базу восемь
// раз незачем. Нулевые и неназванные не возвращаем вовсе — «сумму не
// назвали» и «проект бесплатный» здесь разные утверждения, и пустая
// карта говорит первое.
func (r *Repo) projectCosts(ctx context.Context, clientID uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := r.db.Query(ctx, `
SELECT b.project_id, b.project_cost
FROM project_billing b
JOIN projects pr ON pr.id = b.project_id
WHERE pr.client_user_id = $1 AND b.project_cost > 0`, clientID)
	if err != nil {
		return nil, fmt.Errorf("project costs: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int64, 4)
	for rows.Next() {
		var (
			id   uuid.UUID
			cost int64
		)
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, fmt.Errorf("scan project cost: %w", err)
		}
		out[id] = cost
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
SELECT project_id, platform, SUM(views)::bigint FROM (`+clientPlatformStatsSQL+`
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

// clientPlatformStatsSQL — строки статистики по (проект, площадка) для
// всех проектов заказчика. $1 — заказчик.
//
// Две половины, потому что источник разный и это принципиально:
// подытоженный период отдаёт числа из среза (они заморожены), всё
// остальное — живые, по последнему снимку каждой ссылки. Ролик попадает
// ровно в одну половину: выкладка принадлежит одному периоду.
//
// Репосты отдаются как есть, вместе с NULL: ноль репостов и «площадка их
// не отдаёт» — разные утверждения, и склеить их здесь значило бы
// потерять звёздочку у вовлечённости.
const clientPlatformStatsSQL = `
    -- Подытоженные периоды: числа из среза.
    SELECT pp.project_id, v.platform,
           COALESCE(v.views, 0) AS views, COALESCE(v.likes, 0) AS likes,
           COALESCE(v.comments, 0) AS comments, v.shares
    FROM project_period_views v
    JOIN project_periods pp ON pp.id = v.period_id AND pp.status = 'locked'
    JOIN projects p ON p.id = pp.project_id
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'

    UNION ALL

    -- Всё, что ещё не подытожено: живые числа.
    SELECT p.id, l.platform,
           COALESCE(cur.views, 0), COALESCE(cur.likes, 0),
           COALESCE(cur.comments, 0), cur.shares
    FROM projects p
    JOIN project_publications pub ON pub.project_id = p.id AND pub.status <> 'cancelled'
    JOIN publication_links l ON l.publication_id = pub.id
    LEFT JOIN LATERAL (
        SELECT views, likes, comments, shares FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE p.client_user_id = $1 AND p.is_test = FALSE AND p.status <> 'cancelled'
      AND NOT EXISTS (
          SELECT 1 FROM project_period_publications spp
          JOIN project_periods pp2 ON pp2.id = spp.period_id AND pp2.status = 'locked'
          WHERE spp.publication_id = pub.id
      )
`

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
// periodForOverview — период проекта для сводки, дёшево.
//
// Обычный s.Period сначала материализует периоды, а это пишущая
// транзакция с advisory-локом по проекту. На сводке заказчика она
// выполнялась для КАЖДОГО проекта на каждом GET и гарантированно
// встречалась на том же локе с фоновым LockDuePeriods.
//
// Здесь сперва пробуем прочитать уже заведённый период, и только если
// его нет — идём полной дорогой. Второй случай редкий: периоды заводит
// часовой тикер, а не читатель.
func (s *Service) periodForOverview(
	ctx context.Context, projectID uuid.UUID, now time.Time,
) (ProjectPeriod, error) {
	p, err := s.repo.PeriodOn(ctx, projectID, now)
	if err == nil && !p.EndsOn.Before(dayOf(now)) {
		// Найденный период действительно накрывает сегодня. PeriodOn
		// отбирает по starts_on <= день и без этой проверки отдал бы
		// ПРОШЛЫЙ период, если текущий ещё не заведён, — то есть
		// молча показал бы устаревшие числа вместо материализации.
		return p, nil
	}
	if err != nil && !errors.Is(err, ErrNoPeriods) {
		return ProjectPeriod{}, err
	}
	return s.Period(ctx, projectID, 0, now)
}

func (s *Service) ClientOverview(ctx context.Context, clientID uuid.UUID, rng string, now time.Time) (ClientOverview, error) {
	w, ok := newDashWindow(rng, now)
	if !ok {
		return ClientOverview{}, fmt.Errorf("%w: неизвестное окно %q, бывают: %v",
			ErrInvalidInput, rng, RangeValues)
	}
	out := ClientOverview{
		Projects:    []OverviewProject{},
		Series:      []OverviewPoint{},
		Platforms:   []OverviewPlatform{},
		TopVideos:   []OverviewTopVideo{},
		GeneratedAt: now,
		Range:       w.Name,
		RangeLabel:  w.Label,
		Views:       OverviewViews{ByPlatform: map[string]int64{}},
	}
	// Все пять площадок всегда: пропавший столбик читается как сбой.
	for _, p := range publications.AllPlatforms {
		out.Views.ByPlatform[p] = 0
	}

	clientProjects, err := s.repo.ClientProjects(ctx, clientID, overviewProjectsLimit)
	if err != nil {
		return out, err
	}
	out.ProjectsTotal = len(clientProjects)
	if len(clientProjects) == 0 {
		// Проектов нет — но окно, пять площадок и признак «сравнивать не
		// с чем» отдать всё равно надо: экран рисуется по тем же ключам.
		return out, s.fillDashboard(ctx, &out, clientID, w)
	}

	viewsByProject, err := s.repo.platformViews(ctx, clientID)
	if err != nil {
		return out, err
	}
	lockedByProject, approx, err := s.repo.lockedTotals(ctx, clientID)
	if err != nil {
		return out, err
	}
	// Стоимости проектов, названные менеджером. Нужны только там, где
	// начислений не бывает (см. costBasis ниже).
	costByProject, err := s.repo.projectCosts(ctx, clientID)
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

	var ladder tariffLadder
	// Из чего считается цена просмотра по всем проектам разом. Не
	// равно out.Money.Total: у проекта без креаторов деньги в сводке
	// нулевые (начислений нет), а стоимость — есть.
	var costBasisTotal int64
	for _, pr := range clientProjects {
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
		period, perr := s.periodForOverview(ctx, pr.ID, now)
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
			// Считаем ВСЕ неподытоженные периоды, а не только идущий.
			//
			// Период, который кончился, но ждёт отсечки, две недели не
			// попадал никуда: подытоженным он ещё не стал, текущим уже
			// не был, — и проект на эти две недели дешевел на целый
			// период прямо на глазах у заказчика.
			pending := []ProjectPeriod{}
			if !period.IsLocked() {
				pending = append(pending, period)
			}
			awaiting, werr := s.repo.PeriodsAwaitingLock(ctx, pr.ID, now)
			if werr != nil {
				return out, werr
			}
			for _, p := range awaiting {
				if p.ID != period.ID {
					pending = append(pending, p)
				}
			}
			if len(pending) > 0 {
				terms, terr := s.repo.Terms(ctx, pr.ID)
				if terr != nil {
					return out, terr
				}
				for _, p := range pending {
					accruals, aerr := s.accrualsOrPreview(ctx, pr.ID, p, terms)
					if aerr != nil {
						return out, aerr
					}
					current := totals(accruals).Total
					row.Total += current
					out.Money.Current += current
					// Лесенку показываем по идущему периоду: она
					// объясняет ЦЕНУ СЕГОДНЯ, а сложенная по двум
					// периодам ступень не сойдётся ни с одним из них.
					if p.ID == period.ID {
						ladder.add(terms, accruals)
					}
				}
			}
		}

		// Законченный проект называется законченным, чем бы ни кончился
		// его последний период: «идёт» у сданного проекта — прямая
		// неправда, и по этому же признаку заказчик отбирает строки в
		// итоги по сделанному.
		if pr.Done {
			row.State = OverviewCompleted
			row.CompletedAt = pr.CompletedAt
		}

		// Цена просмотра — из того, во что проект обошёлся заказчику.
		//
		// У проекта с креаторами это сумма начислений: счёт складывается
		// из работы людей. У проекта без креаторов людей нет, начислений
		// не бывает, и row.Total — честный ноль; но проект при этом
		// стоит денег, и сколько именно — назвал менеджер одним числом.
		// По нему считает СПВ его карточка (publications.fillCost), и
		// заказчик обязан видеть ТУ ЖЕ цену: два разных ответа на
		// главный к такому проекту вопрос хуже, чем ни одного.
		costBasis := row.Total
		if projects.FeaturesOf(projects.ProjectKind(pr.Kind)).HasManualCost {
			costBasis = costByProject[pr.ID]
		}
		costBasisTotal += costBasis
		row.CostPer1000 = costPer1000(costBasis, row.Views)
		out.Projects = append(out.Projects, row)
	}

	out.Money.Total = out.Money.Locked + out.Money.Current
	// Стоимость тысячи — из фактических сумм и фактических просмотров.
	// Средняя ставка по тарифам соврала бы: у проектов разные версии
	// условий, и цена тысячи у них разная.
	//
	// Складываем ту же основу, что и по строкам: иначе у заказчика, у
	// которого есть проект без креаторов, общая цена просмотра не
	// сойдётся с колонкой рядом.
	out.CostPer1000 = costPer1000(costBasisTotal, out.Views.Total)
	out.Tariff = ladder.result(out.Money.Total, out.Views.Total)

	if err := s.fillDashboard(ctx, &out, clientID, w); err != nil {
		return out, err
	}
	if err := s.fillMarket(ctx, &out); err != nil {
		return out, err
	}
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
// @Description
// @Description Дашборд считается за СКОЛЬЗЯЩЕЕ окно от сегодня: week — 7 дней,
// @Description month — 30, quarter — 90. Итоги (просмотры, взаимодействия, ER,
// @Description площадки) — за всё время; приросты — окно против предыдущего
// @Description окна такой же длины. Если сравнивать не с чем, поля прироста
// @Description нет вовсе: ноль означал бы «не выросло».
// @Tags     client-billing
// @Produce  json
// @Security BearerAuth
// @Param    range query string false "окно дашборда: week | month | quarter (по умолчанию month)" Enums(week,month,quarter)
// @Success  200 {object} ClientOverview
// @Failure  400 {object} errorResponse "invalid_input — неизвестное окно"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/overview [get]
func (h *Handler) ClientOverviewHandler(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	// Пустой параметр — месяц: дашборд открывают без вопросов, а
	// незнакомое значение это уже опечатка, о которой надо сказать.
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = RangeMonth
	}
	out, err := h.svc.ClientOverview(r.Context(), uid, rng, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ProjectBenchmark — «типичный ролик» для кабинета креатора и
// обезличенный ориентир проекта.
//
// Лесенка от общего к личному, целиком на сервере:
//
//  1. своя история, если зрелых роликов хватает — самое точное;
//  2. иначе медиана проекта, но только пока она остаётся агрегатом;
//  3. иначе значение по умолчанию — из версионируемого справочника
//     порогов, а не из константы в коде (fallbackViews).
//
// Фронту выбор не отдаётся намеренно: вторая копия правила в браузере
// разойдётся с этой при первой же правке, и обещание «до ступени
// столько-то» станет зависеть от того, чей код старше.
//
// Зрелым считается ролик старше двух недель — тех же, что у отсечки
// периода: пока он растёт, сравнивать его с отлежавшимися нечестно.
// Возраст берём от фактической даты публикации, а где её нет — от сдачи
// ссылки, как и везде.
func (r *Repo) ProjectBenchmark(ctx context.Context, projectID, creatorID uuid.UUID, now time.Time, fallbackViews int64) (*ProjectBenchmark, error) {
	cutoff := now.Add(-matureVideoAge)

	// Одна выкладка — один ролик: просмотры суммируются по площадкам, и
	// считать ролик пять раз значило бы завысить и медианы, и пороги.
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
    (SELECT COUNT(*)::int FROM mature WHERE creator_user_id = $2),
    -- Медиана самого спрашивающего. NULL, если зрелых роликов у него нет.
    (SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY views)
     FROM mature WHERE creator_user_id = $2),
    -- Доля роликов проекта, которые слабее его медианного.
    (SELECT COUNT(*)::int FROM mature
     WHERE views < (SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY views)
                    FROM mature WHERE creator_user_id = $2))
FROM mature`

	var (
		videos, creators, myVideos, weaker int
		projectMedian                      int64
		myMedian                           *float64
	)
	if err := r.db.QueryRow(ctx, q, projectID, creatorID, cutoff).
		Scan(&videos, &creators, &projectMedian, &myVideos, &myMedian, &weaker); err != nil {
		return nil, fmt.Errorf("project benchmark: %w", err)
	}

	out := &ProjectBenchmark{
		MyMatureVideos:     myVideos,
		TypicalVideoViews:  fallbackViews,
		TypicalVideoSource: TypicalFromDefault,
	}

	// Порог обезличивания: мало роликов или мало людей — это уже не
	// агрегат, а чужой показатель, и в ответе его быть не должно. На
	// «типичный ролик» это влияет тоже: опереться на такую медиану
	// значит опереться на соседа.
	anonymous := videos >= medianMinVideos && creators >= medianMinCreators
	if anonymous {
		median := projectMedian
		count := videos
		out.ProjectMedianViews = &median
		out.MatureVideos = &count
		if myMedian != nil {
			p := weaker * 100 / videos
			out.MyPercentile = &p
		}
		out.TypicalVideoViews = projectMedian
		out.TypicalVideoSource = TypicalFromProject
	}

	// Своя история точнее всего — она и побеждает, если её хватает.
	if myVideos >= creatorMinMatureVideos && myMedian != nil {
		out.TypicalVideoViews = int64(*myMedian)
		out.TypicalVideoSource = TypicalFromCreator
	}

	// Медиана может выйти нулевой: ролики есть, просмотров по ним нет.
	// Ноль как «типичный ролик» сломал бы любой расчёт «сколько
	// осталось», поэтому в этом случае честнее значение по умолчанию.
	if out.TypicalVideoViews <= 0 {
		out.TypicalVideoViews = fallbackViews
		out.TypicalVideoSource = TypicalFromDefault
	}
	return out, nil
}
