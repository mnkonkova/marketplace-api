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
