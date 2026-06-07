package transcode

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Метрики транскод-пайплайна. Шкала latency 0.5s-300s покрывает
// realistic-сценарии: 480p × 8 сек с veryfast ≈ 10-30 сек на 1 vCPU.
var (
	successTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "transcode_success_total",
		Help: "Transcode pipeline successful runs.",
	})

	// reason: permanent | network | timeout | other. Кардинальность 4.
	errorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "transcode_errors_total",
			Help: "Transcode pipeline failures by reason category.",
		},
		[]string{"reason"},
	)

	durationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "transcode_duration_seconds",
		Help:    "End-to-end transcode duration (download + ffmpeg + upload).",
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 25, 50, 100, 200, 300},
	})

	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "transcode_queue_depth",
		Help: "Portfolio items waiting for preview (preview_status='pending').",
	})
)

// RefreshQueueDepth — обновляет gauge transcode_queue_depth. Вызывается
// периодически воркером (см. cmd/worker/main.go). Партиальный индекс
// portfolio_items_pending_preview_idx (00020) держит запрос дешёвым:
// count проходит по индексу размером десятки записей.
func RefreshQueueDepth(ctx context.Context, db *pgxpool.Pool) error {
	var n int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM portfolio_items WHERE preview_status = 'pending'`).Scan(&n); err != nil {
		return err
	}
	queueDepth.Set(float64(n))
	return nil
}
