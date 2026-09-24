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
	StatLagSeconds      int64 `json:"stat_lag_seconds"`
	PublicationsOverdue int   `json:"publications_overdue"`
}

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
  AND pr.kind = 'creators_turnkey'
  AND NOT EXISTS (
      SELECT 1 FROM publication_date_requests dr
      WHERE dr.publication_id = p.id AND dr.status = 'pending'
  )`, now).Scan(&g.PublicationsOverdue); err != nil {
		return g, fmt.Errorf("gauge publications overdue: %w", err)
	}

	linksStale.Set(float64(g.LinksStale))
	statLagSeconds.Set(float64(g.StatLagSeconds))
	publicationsOverdue.Set(float64(g.PublicationsOverdue))
	return g, nil
}

// RefreshGauges — обёртка сервиса для воркера.
func (s *Service) RefreshGauges(ctx context.Context, now time.Time) (GaugeSnapshot, error) {
	return s.repo.RefreshGauges(ctx, now)
}
