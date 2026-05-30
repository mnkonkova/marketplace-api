// Package notifications — outbox-handler, который доставляет события
// project.* и client_invite.* во внешний n8n через HTTP webhook.
// n8n внутри одного workflow Switch-нодом маршрутизирует по event_type
// в нужную ветку (email/Telegram/идемпотентность через Redis).
//
// Архитектурно — тонкий мост между outbox.Worker'ом и HTTP. Бизнес-логика
// нотификаций (шаблоны писем, выбор провайдера) живёт в n8n, не здесь.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Config — параметры диспатчера.
type Config struct {
	// WebhookBaseURL — полный URL endpoint'а n8n. Пример:
	//   https://automation.example.com/webhook/project-events
	// Если пуст — Dispatcher не создаётся (worker логирует warning и
	// не подключает handler).
	WebhookBaseURL string

	// HTTPTimeout — таймаут одного POST'а. n8n обычно отвечает за <1с
	// (он же кладёт в очередь сам), 10с — щедрый запас.
	HTTPTimeout time.Duration

	// ExtraHeaders — например, X-Webhook-Token для аутентификации. n8n
	// Webhook node поддерживает header-аутентификацию.
	ExtraHeaders map[string]string

	// Registerer — куда регистрировать метрики. nil → prometheus.DefaultRegisterer
	// (прод-поведение). В тестах подкладываем prometheus.NewRegistry(),
	// чтобы избежать «already registered» при создании нескольких
	// диспатчеров.
	Registerer prometheus.Registerer
}

// Dispatcher реализует outbox.Handler. Метрики:
//   - crm_webhook_delivered_total{event_type} — 2xx ответы;
//   - crm_webhook_failed_total{event_type,reason} — все не-2xx и
//     network-failure'ы (reason ∈ http_5xx | http_4xx | network).
//     http_4xx считается failed для observability, но НЕ ретраится
//     (logical-fail на стороне n8n — повторение бесполезно).
type Dispatcher struct {
	httpClient *http.Client
	url        string
	headers    map[string]string
	logger     *slog.Logger

	delivered *prometheus.CounterVec
	failed    *prometheus.CounterVec
}

// envelope — то, что мы шлём в n8n. event_id (=outbox aggregate_id)
// нужен для idempotency-check в Redis-ноде flow'а.
type envelope struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Aggregate string          `json:"aggregate"`
	Payload   json.RawMessage `json:"payload"`
}

// NewDispatcher создаёт диспатчер и регистрирует метрики (один раз).
// logger — для warning'ов при не-2xx ответах.
func NewDispatcher(cfg Config, logger *slog.Logger) *Dispatcher {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	reg := cfg.Registerer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	delivered := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_webhook_delivered_total",
		Help: "Количество outbox-событий, успешно доставленных в n8n.",
	}, []string{"event_type"})
	failed := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "crm_webhook_failed_total",
		Help: "Количество outbox-событий, не доставленных в n8n (для алертов).",
	}, []string{"event_type", "reason"})
	reg.MustRegister(delivered, failed)
	return &Dispatcher{
		httpClient: &http.Client{Timeout: cfg.HTTPTimeout},
		url:        cfg.WebhookBaseURL,
		headers:    cfg.ExtraHeaders,
		logger:     logger,
		delivered:  delivered,
		failed:     failed,
	}
}

// Handle — сигнатура совместима с outbox.Handler. aggregate-имя не
// передаётся параметром (worker раздаёт хендлеры по aggregate), но
// требуется в payload для switch'а в n8n — берём через aggregateID-
// ассоциацию.
//
// Возвращает ошибку только при network-failure / 5xx — outbox их
// ретрайит с backoff'ом. 4xx считается «логически отвалилось», не
// ретраим (записываем метрику).
func (d *Dispatcher) Handle(aggregate string) func(ctx context.Context, aggregateID, eventType string, payload []byte) error {
	return func(ctx context.Context, aggregateID, eventType string, payload []byte) error {
		body, err := json.Marshal(envelope{
			EventID:   aggregateID,
			EventType: eventType,
			Aggregate: aggregate,
			Payload:   payload,
		})
		if err != nil {
			d.failed.WithLabelValues(eventType, "marshal").Inc()
			return fmt.Errorf("marshal envelope: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
		if err != nil {
			d.failed.WithLabelValues(eventType, "build_request").Inc()
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range d.headers {
			req.Header.Set(k, v)
		}

		resp, err := d.httpClient.Do(req)
		if err != nil {
			d.failed.WithLabelValues(eventType, "network").Inc()
			return fmt.Errorf("post to n8n: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		// Считываем тело, чтобы re-use соединения; ограничим объём — n8n
		// может вернуть HTML на 500.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			d.delivered.WithLabelValues(eventType).Inc()
			return nil
		case resp.StatusCode >= 500:
			d.failed.WithLabelValues(eventType, "http_5xx").Inc()
			return fmt.Errorf("n8n returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
		default:
			// 4xx — не ретраим (запрос некорректный или idempotency drop в n8n).
			d.failed.WithLabelValues(eventType, "http_4xx").Inc()
			d.logger.Warn("n8n webhook 4xx — drop without retry",
				"event_type", eventType,
				"status", resp.StatusCode,
				"body", truncate(string(respBody), 200),
			)
			return nil
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
