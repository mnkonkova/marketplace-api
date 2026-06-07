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
		Help: "Portfolio items waiting for preview: status='pending' + stuck 'processing' (updated_at > 15m ago).",
	})

	// stuckProcessing — отдельный gauge только для застрявших processing'ов.
	// Высокий и устойчивый = воркер крашится между claim и commitReady;
	// auto-recovery в claim() (E1/T1) их перехватит, но gauge даёт
	// раннее предупреждение.
	stuckProcessing = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "transcode_stuck_processing",
		Help: "Portfolio items in 'processing' state longer than 15 min (worker crash residue).",
	})
)

// RefreshQueueDepth — обновляет transcode_queue_depth и
// transcode_stuck_processing. Вызывается периодически воркером
// (см. cmd/worker/main.go). Партиальный индекс
// portfolio_items_pending_preview_idx (00020) ускоряет pending-запрос;
// stuck-processing запрос проходит по основной B-tree (мало строк).
func RefreshQueueDepth(ctx context.Context, db *pgxpool.Pool) error {
	var pending, stuck int64
	if err := db.QueryRow(ctx, `
SELECT
  (SELECT COUNT(*) FROM portfolio_items WHERE preview_status = 'pending'),
  (SELECT COUNT(*) FROM portfolio_items
    WHERE preview_status = 'processing'
      AND updated_at < now() - INTERVAL '15 minutes')
`).Scan(&pending, &stuck); err != nil {
		return err
	}
	queueDepth.Set(float64(pending + stuck))
	stuckProcessing.Set(float64(stuck))
	return nil
}
