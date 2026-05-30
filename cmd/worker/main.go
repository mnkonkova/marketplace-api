package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"marketpclce/internal/config"
	"marketpclce/internal/invites"
	"marketpclce/internal/mailer"
	"marketpclce/internal/notifications"
	"marketpclce/internal/outbox"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
	"marketpclce/internal/projects"
	"marketpclce/internal/search"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(rootCtx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	esClient := es.New(cfg.OpenSearchURL)
	// EnsureIndex с ретраями: при холодном старте compose OS может быть ещё
	// не готов, EOF/connection-reset — норма в первые секунды. Раньше worker
	// падал os.Exit(1), оставляя outbox без обработчика. Ждём до 60 c
	// (60×1 c), затем — fatal: что-то серьёзно сломано.
	if err := ensureIndexWithRetry(rootCtx, esClient, cfg.OpenSearchIndexProfile, search.IndexMapping(), "specialists"); err != nil {
		slog.Error("ensure index", "err", err)
		os.Exit(1)
	}
	if err := ensureIndexWithRetry(rootCtx, esClient, cfg.OpenSearchIndexFeedVideos, search.FeedVideoMapping(), "feed_videos"); err != nil {
		slog.Error("ensure feed_videos index", "err", err)
		os.Exit(1)
	}

	repo := search.NewRepo(pool)
	indexer := search.NewIndexer(repo, esClient, cfg.OpenSearchIndexProfile)
	feedIndexer := search.NewFeedIndexer(repo, esClient, cfg.OpenSearchIndexFeedVideos)

	// Bootstrap: если feed_videos пустой (первый запуск после деплоя Stage 2
	// или после ручного reset'а индекса) — прогоняем всех опубликованных спецов
	// один раз. Дальше держим индекс актуальным через outbox-события.
	if empty, err := feedIndexer.IsEmpty(rootCtx); err != nil {
		slog.Warn("feed_videos isEmpty check failed (skipping bootstrap)", "err", err)
	} else if empty {
		n, err := feedIndexer.Bootstrap(rootCtx)
		if err != nil {
			slog.Error("feed_videos bootstrap failed", "err", err)
		} else {
			slog.Info("feed_videos bootstrapped", "specialists", n)
		}
	}

	specialistHandler := func(ctx context.Context, aggregateID, eventType string, _ []byte) error {
		uid, err := uuid.Parse(aggregateID)
		if err != nil {
			return err
		}
		switch eventType {
		case outbox.EventSpecialistDeleted:
			if err := indexer.Delete(ctx, uid); err != nil {
				return err
			}
			return feedIndexer.DeleteByUser(ctx, uid)
		default:
			if err := indexer.Reconcile(ctx, uid); err != nil {
				return err
			}
			return feedIndexer.ReconcileVideos(ctx, uid)
		}
	}

	// Mailer: без UNISENDER_API_KEY отправка отключена — события email.*
	// будут падать с ошибкой и оставаться в outbox для ретрая. В деве это
	// норм: ставите ключ и события долетают; в проде — обязательно задать.
	var sender mailer.Sender
	if cfg.UnisenderAPIKey != "" {
		sender = mailer.NewUnisenderGo(mailer.UnisenderGoConfig{
			APIKey:    cfg.UnisenderAPIKey,
			BaseURL:   cfg.UnisenderAPIBaseURL,
			FromEmail: cfg.UnisenderFromEmail,
			FromName:  cfg.UnisenderFromName,
		})
		slog.Info("mailer ready", "provider", "unisender_go", "from", cfg.UnisenderFromEmail)
	} else {
		slog.Warn("mailer disabled (UNISENDER_API_KEY empty) — email.* events will fail and stay in outbox")
	}

	emailHandler := func(ctx context.Context, _, eventType string, payload []byte) error {
		switch eventType {
		case outbox.EventEmailVerifySend:
			var p outbox.EmailVerifyPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("decode email payload: %w", err)
			}
			// Без mailer'а (типично для локального запуска без Unisender)
			// логируем verify-URL в stdout — дев берёт его руками. Помечаем
			// событие как обработанное, чтобы не зависало в outbox-ретраях.
			if sender == nil {
				slog.Info("verify-email (mailer disabled, copy this URL manually)",
					"to", p.To,
					"url", p.BaseURL+"/verify?token="+p.Token,
				)
				return nil
			}
			subj, plain := renderVerifyEmail(p)
			return sender.Send(ctx, mailer.Message{
				To:      p.To,
				ToName:  p.ToName,
				Subject: subj,
				Plain:   plain,
			})
		case outbox.EventEmailPasswordResetSend:
			var p outbox.EmailPasswordResetPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("decode password reset payload: %w", err)
			}
			if sender == nil {
				slog.Info("password-reset (mailer disabled, copy this URL manually)",
					"to", p.To,
					"url", p.BaseURL+"/auth/reset?token="+p.Token,
				)
				return nil
			}
			subj, plain := renderPasswordResetEmail(p)
			return sender.Send(ctx, mailer.Message{
				To:      p.To,
				ToName:  p.ToName,
				Subject: subj,
				Plain:   plain,
			})
		default:
			slog.Warn("unknown email event", "type", eventType)
			return nil
		}
	}

	handlers := map[string]outbox.Handler{
		outbox.AggregateSpecialist: specialistHandler,
		outbox.AggregateEmail:      emailHandler,
	}

	// n8n-диспатчер: один HTTP-клиент шлёт project.* и client_invite.*
	// на общий webhook. n8n внутри workflow роутит Switch-нодой по
	// event_type. Если N8N_WEBHOOK_BASE_URL пуст — диспатчер отключён
	// (события остаются в outbox и через MaxAttempts уйдут в DLQ).
	if cfg.N8nWebhookBaseURL != "" {
		dispatcher := notifications.NewDispatcher(notifications.Config{
			WebhookBaseURL: cfg.N8nWebhookBaseURL,
			HTTPTimeout:    cfg.N8nHTTPTimeout,
		}, logger)
		handlers[projects.AggregateProject] = dispatcher.Handle(projects.AggregateProject)
		handlers[invites.AggregateClientInvite] = dispatcher.Handle(invites.AggregateClientInvite)
		slog.Info("n8n dispatcher ready",
			"webhook", cfg.N8nWebhookBaseURL,
			"timeout", cfg.N8nHTTPTimeout,
		)
	} else {
		slog.Warn("n8n dispatcher disabled (N8N_WEBHOOK_BASE_URL empty) — project/client_invite events will DLQ after retries")
	}

	worker := outbox.NewWorker(pool, logger, handlers,
		outbox.Config{
			MaxAttempts:     cfg.OutboxMaxAttempts,
			BackoffCap:      cfg.OutboxBackoffCap,
			Retention:       cfg.OutboxRetention,
			CleanupInterval: cfg.OutboxCleanupInterval,
		})

	// /metrics для alloy. Отдельный listener, чтобы не путать с api:8080 и
	// чтобы worker оставался без бизнес-API. /healthz нужен compose'у — без
	// него health-check'и не зацепятся.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	metricsSrv := &http.Server{
		Addr:              cfg.WorkerMetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("worker metrics listening", "addr", cfg.WorkerMetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("worker metrics server", "err", err)
		}
	}()
	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	if err := worker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("worker stopped", "err", err)
		os.Exit(1)
	}
	slog.Info("worker bye")
}

// renderVerifyEmail — собирает subject + plain-text тело письма
// подтверждения почты. Plain-only для MVP: меньше шансов уехать в спам,
// быстрее доставка, нет HTML/CSS-зоопарка под разные клиенты.
func renderVerifyEmail(p outbox.EmailVerifyPayload) (subject, plain string) {
	greeting := "Привет!"
	if p.ToName != "" {
		greeting = "Привет, " + p.ToName + "!"
	}
	link := p.BaseURL + "/verify?token=" + p.Token
	subject = "Подтвердите почту на marketpclce"
	plain = greeting + "\n\n" +
		"Подтвердите, что это вы зарегистрировались на marketpclce.\n" +
		"Перейдите по ссылке:\n\n" +
		link + "\n\n" +
		"Ссылка действует 24 часа.\n" +
		"Если это не вы — просто проигнорируйте письмо.\n\n" +
		"— marketpclce"
	return subject, plain
}

// renderPasswordResetEmail — письмо со ссылкой сброса пароля. Plain-only
// по тем же причинам что у verify: меньше шансов в спам, нет HTML-зоопарка.
func renderPasswordResetEmail(p outbox.EmailPasswordResetPayload) (subject, plain string) {
	greeting := "Привет!"
	if p.ToName != "" {
		greeting = "Привет, " + p.ToName + "!"
	}
	link := p.BaseURL + "/auth/reset?token=" + p.Token
	subject = "Сброс пароля на marketpclce"
	plain = greeting + "\n\n" +
		"Кто-то запросил сброс пароля для вашего аккаунта.\n" +
		"Если это вы — перейдите по ссылке и задайте новый пароль:\n\n" +
		link + "\n\n" +
		"Ссылка действует 1 час.\n" +
		"Если это не вы — просто проигнорируйте письмо, пароль не изменится.\n\n" +
		"— marketpclce"
	return subject, plain
}

// ensureIndexWithRetry — EnsureIndex с экспоненциальным ожиданием.
// На холодном старте docker compose OpenSearch ещё может не принимать
// соединения; раньше worker падал с os.Exit(1) и outbox-события копились
// без обработчика. 60 секунд (60×1 c) с запасом покрывают cold start.
func ensureIndexWithRetry(ctx context.Context, esClient *es.Client, index string, mapping map[string]any, label string) error {
	const maxAttempts = 60
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if err := esClient.EnsureIndex(ctx, index, mapping); err != nil {
			lastErr = err
			if i == 0 || i == maxAttempts-1 || i%10 == 0 {
				slog.Warn("ensure index retrying",
					"index", label, "attempt", i+1, "max", maxAttempts, "err", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if i > 0 {
			slog.Info("ensure index ok after retry", "index", label, "attempts", i+1)
		}
		return nil
	}
	return fmt.Errorf("ensure index %q after %d attempts: %w", label, maxAttempts, lastErr)
}
