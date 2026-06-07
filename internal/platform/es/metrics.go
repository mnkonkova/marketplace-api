package es

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Метрики OpenSearch-клиента. Каждый запрос инкрементит счётчик и
// пишет latency в histogram. op — высокоуровневая операция (search,
// index_doc, bulk, ...), а не сырой path, чтобы кардинальность не
// взлетала на динамических _id.
var (
	esRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "es_requests_total",
			Help: "Total OpenSearch HTTP requests by operation and status_class (2xx/4xx/5xx/error).",
		},
		[]string{"op", "status_class"},
	)

	esRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "es_request_duration_seconds",
			Help: "OpenSearch request latency distribution by operation.",
			// 5ms-30s покрывает обычный search (10-50ms) и тяжёлые агрегации (1-10s).
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"op"},
	)
)

// statusClass превращает HTTP-статус в "2xx"/"4xx"/"5xx"; -1 (сетевая
// ошибка/таймаут до получения ответа) → "error". Кардинальность 4 →
// безопасно для long-term storage.
func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "error"
	}
}
