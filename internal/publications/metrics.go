package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Метрики проектной страницы.
//
// Зачем они здесь вообще: сбор статистики — единственная часть системы,
// которая ломается МОЛЧА. Ролик не отдал просмотры — в отчёте просто ноль,
// и никто не узнает, пока клиент не спросит «почему цифры не растут».
// Ошибки в логах при этом может не быть вовсе: instacurl честно ответит
// ok=false, а у поставщика просто кончились кредиты.
var (
	// collectTotal — исходы обходов. result: ok | no_data.
	// Разрез по площадке нужен, чтобы отличить «всё лежит» от «отвалился
	// парсер одной площадки» — второе гораздо вероятнее.
	collectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_stat_collect_total",
		Help: "Stats collection attempts by platform and result.",
	}, []string{"platform", "result"})

	// accountScanTotal — обходы аккаунтов креаторов. result: ok | no_data.
	//
	// Отдельно от collectTotal, хотя сервис тот же: это ДРУГОЙ расход.
	// Сбор по ссылкам растёт от числа сданных роликов и сам собой
	// затихает по графику 1→2→4→8, а обход аккаунтов стоит ровно кредит
	// на аккаунт в сутки, пока проект идёт, — и не зависит ни от чего,
	// кроме числа строк в project_accounts. Сложенные в один счётчик,
	// эти два расхода нельзя ни развести по причинам, ни спланировать.
	accountScanTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_account_scan_total",
		Help: "Creator account scans by platform and result.",
	}, []string{"platform", "result"})

	// collectErrorsTotal — сервис не ответил вовсе. reason отделяет
	// недоступность от отказа по ключу и от лимита: чинятся они по-разному.
	collectErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_stat_collect_errors_total",
		Help: "Failed calls to the stats collection service by reason.",
	}, []string{"reason"})

	// collectSaturatedTotal — проходы, в которых выборка вернула РОВНО
	// лимит.
	//
	// Это единственный ранний признак того, что мы упёрлись в потолок.
	// Полная пачка означает «было что брать сверх лимита»: очередь не
	// разгребается, и ссылки начнут отставать. Отставание видно по
	// crm_links_stale, но оно загорается на сутки позже — когда цифры в
	// отчёте уже протухли.
	collectSaturatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crm_stat_collect_saturated_total",
		Help: "Collection passes that filled the whole batch, meaning the queue is longer than capacity.",
	})

	// linksStale — живые ссылки, которые давно должны были обойтись,
	// но не обошлись. Растёт, когда сбор встал.
	linksStale = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crm_links_stale",
		Help: "Links overdue for collection by more than a day.",
	})

	// linksNeverCollected — сданные ссылки, по которым сбор не проходил
	// НИ РАЗУ.
	//
	// Отдельно от linksStale, хотя обе про «сбор не идёт». Отставание
	// говорит «цифры устаревают», и у него порог в сутки: цифра вчера
	// есть, просто она вчерашняя. Здесь цифры нет вовсе, а в отчёте
	// вместо неё ноль — и ноль читается как «ролик никто не смотрит».
	// Это худшая подмена из возможных, поэтому и порог другой: не сутки,
	// а пара часов.
	//
	// Именно этого показателя не хватило 1 октября 2026. В окружении
	// прода не было INSTACURL_URL (стояла INSTACURL_HOST — переменная
	// Alloy для скрейпа), воркер честно написал в лог «stats collection
	// disabled» и больше ничего. Сбор не шёл, менеджер смотрел на пустые
	// просмотры у вышедшего ролика, и ни один алерт не горел: отставание
	// считается от next_collect_at, а он у свежей ссылки ещё не настал.
	linksNeverCollected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crm_links_never_collected",
		Help: "Submitted links that have never been collected, older than two hours.",
	})

	// statLagSeconds — возраст самой свежей цифры среди живых ссылок.
	// Это ровно то число, которое показывается пользователю как «дата
	// последнего сбора»: если оно растёт, отчёт врёт всё сильнее.
	statLagSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crm_stat_lag_seconds",
		Help: "Age of the freshest collected snapshot across live links.",
	})

	// publicationsOverdue — просроченные выкладки без согласованного
	// переноса. Бизнес-показатель, а не технический: растёт, когда
	// напоминания перестали работать или креаторы перестали сдавать.
	publicationsOverdue = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crm_publications_overdue",
		Help: "Open publications past their due date without a pending date request.",
	})

	// remindersSentTotal — отправленные напоминания по видам.
	remindersSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_reminders_sent_total",
		Help: "Reminders actually sent, by kind.",
	}, []string{"kind"})

	// remindersSuppressedTotal — сколько напоминаний погасил дедуп. Это
	// норма, а не ошибка: показывает, что защита от повторов работает.
	remindersSuppressedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crm_reminders_suppressed_total",
		Help: "Reminders skipped because the same one was already sent today.",
	})
)

// ObserveCollect — исход одного обхода.
func ObserveCollect(platform, result string) {
	collectTotal.WithLabelValues(platform, result).Inc()
}

// ObserveAccountScan — исход одного обхода аккаунта.
func ObserveAccountScan(platform, result string) {
	accountScanTotal.WithLabelValues(platform, result).Inc()
}

// ObserveCollectError — сервис не ответил.
func ObserveCollectError(reason string) {
	collectErrorsTotal.WithLabelValues(reason).Inc()
}

// ObserveCollectSaturated — проход выгреб пачку целиком.
func ObserveCollectSaturated() {
	collectSaturatedTotal.Inc()
}

// GaugeSnapshot — то, что показывают gauge'и. Возвращается ещё и наружу,
// чтобы это можно было проверить тестом, не поднимая Prometheus.
type GaugeSnapshot struct {
	LinksStale          int   `json:"links_stale"`
	LinksNeverCollected int   `json:"links_never_collected"`
	StatLagSeconds      int64 `json:"stat_lag_seconds"`
	PublicationsOverdue int   `json:"publications_overdue"`
}

// neverCollectedGrace — сколько ссылке дозволено прожить без цифр.
//
// Два часа, а не десять минут: ссылка, сданная за минуту до прохода,
// законно ждёт следующего тика, а сам сбор может быть занят очередью.
// Два часа молчания по ссылке, которую НИ РАЗУ не пробовали собрать,
// означают, что сбор не поднялся вовсе.
const neverCollectedGrace = 2 * time.Hour

// RefreshGauges — пересчитать бизнес-gauge'и. Дешёвые запросы по узким
// индексам, зовутся периодически из воркера.
func (r *Repo) RefreshGauges(ctx context.Context, now time.Time) (GaugeSnapshot, error) {
	var g GaugeSnapshot

	// Ссылки, просроченные к обходу больше чем на сутки. 'infinity' у
	// снятых с обхода сюда не попадает по построению.
	if err := r.db.QueryRow(ctx, `
SELECT count(*)
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
JOIN projects pr ON pr.id = p.project_id
WHERE l.next_collect_at < $1::timestamptz - interval '1 day'
  AND p.status <> 'cancelled'
  AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)`,
		now).Scan(&g.LinksStale); err != nil {
		return g, fmt.Errorf("gauge links stale: %w", err)
	}

	// Сданные ссылки без единого снимка. Снятые с обхода и мёртвые
	// проекты исключены теми же условиями, что и в отставании: там цифр
	// нет законно.
	if err := r.db.QueryRow(ctx, `
SELECT count(*)
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
JOIN projects pr ON pr.id = p.project_id
WHERE l.last_collected_at IS NULL
  -- И попытки не было: ссылка, по которой сбор ХОДИЛ и получил отказ,
  -- сюда не относится — про неё говорит причина в отчёте, а алерт
  -- тревожит о том, что сбор не поднялся вовсе. Иначе один
  -- неподдержанный VK держал бы алерт горящим вечно.
  AND l.last_collect_try_at IS NULL
  AND l.next_collect_at <> `+ParkedAt+`
  AND l.submitted_at < $1::timestamptz - $2::interval
  AND p.status <> 'cancelled'
  AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)`,
		now, neverCollectedGrace).Scan(&g.LinksNeverCollected); err != nil {
		return g, fmt.Errorf("gauge links never collected: %w", err)
	}

	// Возраст самой свежей цифры. NULL (ничего не собирали) — это 0, а
	// не бесконечность: пустой проект не должен поднимать алерт.
	//
	// Ограничение по stat_date обязательно: без него запрос читал всю
	// video_stat_daily целиком (Seq Scan) каждые пять минут, 288 раз в
	// сутки, а таблица растёт как «ссылки × дни». Неделя с запасом
	// покрывает самый редкий интервал обхода — 8 дней там, где сбор
	// уже идёт; если свежее недели нет вовсе, лаг и так за порогом
	// алерта.
	var lag *float64
	if err := r.db.QueryRow(ctx, `
SELECT EXTRACT(EPOCH FROM ($1::timestamptz - MAX(d.collected_at)))
FROM video_stat_daily d
JOIN publication_links l ON l.id = d.link_id
JOIN project_publications p ON p.id = l.publication_id
JOIN projects pr ON pr.id = p.project_id
WHERE d.stat_date >= $1::date - 7
  AND p.status <> 'cancelled'
  AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)`,
		now).Scan(&lag); err != nil {
		return g, fmt.Errorf("gauge stat lag: %w", err)
	}
	if lag != nil && *lag > 0 {
		g.StatLagSeconds = int64(*lag)
	}

	if err := r.db.QueryRow(ctx, `
SELECT count(*)
FROM project_publications p
JOIN projects pr ON pr.id = p.project_id
WHERE p.status IN ('planned', 'partial')
  AND p.due_date < $1::date
  -- Оба вида с выкладками: просрочка у проекта без креаторов такая же
  -- просрочка, и gauge, который её не видит, врёт.
  AND pr.kind IN ('creators_turnkey', 'brand_turnkey')
  AND NOT EXISTS (
      SELECT 1 FROM publication_date_requests dr
      WHERE dr.publication_id = p.id AND dr.status = 'pending'
  )`, now).Scan(&g.PublicationsOverdue); err != nil {
		return g, fmt.Errorf("gauge publications overdue: %w", err)
	}

	linksStale.Set(float64(g.LinksStale))
	linksNeverCollected.Set(float64(g.LinksNeverCollected))
	statLagSeconds.Set(float64(g.StatLagSeconds))
	publicationsOverdue.Set(float64(g.PublicationsOverdue))
	return g, nil
}

// RefreshGauges — обёртка сервиса для воркера.
func (s *Service) RefreshGauges(ctx context.Context, now time.Time) (GaugeSnapshot, error) {
	return s.repo.RefreshGauges(ctx, now)
}
