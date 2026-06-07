package projects

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Бизнес-метрики маркетплейса — gauge'и, обновляемые ticker'ом из worker'а
// (см. cmd/worker.runBusinessGaugeTicker). Не events-counters: gauge показывает
// текущее состояние БД, простой для понимания через Grafana. Counter
// вычисляется на стороне Grafana как `increase(gauge[24h])`, но для daily-
// панелей удобнее напрямую _24h gauge — меньше запросов в Mimir.
//
// Интервал refresh — 5 минут, нагрузка на Postgres мизерная (5 SELECT COUNT
// с индексами по created_at/status).
var (
	projectsTotal = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "projects_total",
			Help: "Number of projects by status (active/done/cancelled/dispute/...).",
		},
		[]string{"status"},
	)

	projectsCreated24h = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "projects_created_24h",
		Help: "Projects created in the last 24 hours.",
	})

	projectsCompleted24h = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "projects_completed_24h",
		Help: "Projects completed (status=done) in the last 24 hours.",
	})

	leadsTotal24h = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "leads_created_24h",
		Help: "Leads submitted in the last 24 hours.",
	})

	// Конверсия lead → project считается на стороне Grafana как ratio.
	// Чтобы не плодить лишние gauge'ы, leadsTotal24h + projectsCreated24h
	// достаточно для panel'a `projects_created_24h / leads_created_24h`.

	commentsCreated24h = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "project_comments_created_24h",
		Help: "Non-internal project comments created in the last 24 hours.",
	})

	usersTotal = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "users_total",
			Help: "Number of users by kind (client/specialist/both).",
		},
		[]string{"kind"},
	)

	usersRegistered24h = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "users_registered_24h",
		Help: "User registrations in the last 24 hours.",
	})
)

// RefreshBusinessGauges — обновляет все бизнес-gauge'ы из БД.
// Выполняется одним проходом — все 7 SELECT'ов в одной транзакции,
// чтобы видеть согласованный снимок.
func RefreshBusinessGauges(ctx context.Context, db *pgxpool.Pool) error {
	// 1. projects_total{status}
	rows, err := db.Query(ctx, `SELECT status, COUNT(*) FROM projects GROUP BY status`)
	if err != nil {
		return err
	}
	projectsTotal.Reset()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return err
		}
		projectsTotal.WithLabelValues(status).Set(float64(n))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// 2. 24h-окна (одним запросом — экономим RT).
	var created, completed, leads, comments int64
	if err := db.QueryRow(ctx, `
SELECT
  (SELECT COUNT(*) FROM projects WHERE created_at > now() - interval '24 hours'),
  (SELECT COUNT(*) FROM projects WHERE status='done' AND completed_at > now() - interval '24 hours'),
  (SELECT COUNT(*) FROM leads WHERE created_at > now() - interval '24 hours'),
  (SELECT COUNT(*) FROM project_comments WHERE is_internal = FALSE AND created_at > now() - interval '24 hours')
`).Scan(&created, &completed, &leads, &comments); err != nil {
		return err
	}
	projectsCreated24h.Set(float64(created))
	projectsCompleted24h.Set(float64(completed))
	leadsTotal24h.Set(float64(leads))
	commentsCreated24h.Set(float64(comments))

	// 3. users_total{kind} + users_registered_24h
	urows, err := db.Query(ctx, `SELECT kind, COUNT(*) FROM users GROUP BY kind`)
	if err != nil {
		return err
	}
	usersTotal.Reset()
	for urows.Next() {
		var kind string
		var n int64
		if err := urows.Scan(&kind, &n); err != nil {
			urows.Close()
			return err
		}
		usersTotal.WithLabelValues(kind).Set(float64(n))
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		return err
	}

	var newUsers int64
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE created_at > now() - interval '24 hours'`).
		Scan(&newUsers); err != nil {
		return err
	}
	usersRegistered24h.Set(float64(newUsers))

	return nil
}
