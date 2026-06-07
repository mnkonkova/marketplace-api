package outbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Метрики outbox-воркера. Pending/Dead — gauge'и, обновляются периодическим
// поллом БД (см. Worker.refreshGauges); HandlerErrors/Successes — counter'ы,
// инкрементятся прямо в tick'е по факту обработки.
var (
	pendingGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_pending",
		Help: "Outbox entries waiting for processing (processed_at IS NULL AND dead_at IS NULL).",
	})

	deadGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_dead",
		Help: "Outbox entries quarantined after exceeding max attempts (dead_at IS NOT NULL).",
	})

	// lockableGauge — события, которые worker может выбрать в ближайший
	// tick (next_attempt_at IS NULL или ≤ now()). pending - lockable =
	// "запланированное в будущем": либо leased в Phase 1 (P3, на 10
	// минут), либо ждёт backoff после ошибки handler'a.
	//
	// Алерт OutboxLeaseLeak строится поверх этой пары: если pending
	// высокий, а lockable=0 длительное время — значит lease не
	// освобождается (воркер крашится между Phase 2 и Phase 3 или handler
	// вечно зависает), и при следующем тике не за что хвататься.
	lockableGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_lockable",
		Help: "Outbox entries available for immediate processing (next_attempt_at IS NULL or <= now()).",
	})

	handlerErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "outbox_handler_errors_total",
			Help: "Outbox handler errors by aggregate and event_type.",
		},
		[]string{"aggregate", "event_type"},
	)

	handlerSuccessTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "outbox_handler_success_total",
			Help: "Outbox handler successes by aggregate and event_type.",
		},
		[]string{"aggregate", "event_type"},
	)

	deadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "outbox_dead_total",
			Help: "Outbox entries transitioned to dead state, by aggregate and event_type.",
		},
		[]string{"aggregate", "event_type"},
	)
)
