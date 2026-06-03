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
	"marketpclce/internal/mailer"
	"marketpclce/internal/notifications"
	"marketpclce/internal/outbox"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/profiles"
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

	// n8n-диспатчер для CRM-событий project.*. nil = выключен (события
	// квитируются как no-op, чтобы не зависать в outbox-ретраях).
	n8nDispatcher := notifications.NewWebhookDispatcher(cfg.N8nWebhookURL, cfg.N8nWebhookToken)
	if n8nDispatcher == nil {
		slog.Info("n8n webhook disabled (N8N_WEBHOOK_URL empty) — project.* events будут no-op")
	} else {
		slog.Info("n8n webhook ready", "url", cfg.N8nWebhookURL)
	}

	projectHandler := func(ctx context.Context, aggregateID, eventType string, payload []byte) error {
		// no-op если диспатчер не сконфигурирован — событие считается
		// обработанным, чтобы не копить ретраи на проде без webhook.
		if n8nDispatcher == nil {
			return nil
		}
		return n8nDispatcher.Send(ctx, notifications.Payload{
			EventID:     aggregateID + "/" + eventType,
			Aggregate:   outbox.AggregateProject,
			AggregateID: aggregateID,
			EventType:   eventType,
			Data:        payload,
			OccurredAt:  time.Now().UTC(),
		})
	}

	worker := outbox.NewWorker(pool, logger,
		map[string]outbox.Handler{
			outbox.AggregateSpecialist: specialistHandler,
			outbox.AggregateEmail:      emailHandler,
			outbox.AggregateProject:    projectHandler,
		},
		outbox.Config{
			MaxAttempts:     cfg.OutboxMaxAttempts,
			BackoffCap:      cfg.OutboxBackoffCap,
			Retention:       cfg.OutboxRetention,
			CleanupInterval: cfg.OutboxCleanupInterval,
		})

	// CRM v5 Ф5: периодический таск авто-skip просроченных review-шагов.
	// Длительность дедлайна берётся из cfg.ReviewDeadline (по умолчанию 7
	// дней); интервал сканирования — cfg.ReviewCheckInterval (1 час).
	projectsRepo := projects.NewRepo(pool)
	projectsSvc := projects.NewService(projectsRepo).
		WithReviewDeadline(cfg.ReviewDeadline)
	go runReviewAutoSkipTicker(rootCtx, projectsSvc, cfg.ReviewCheckInterval, logger)
	// CRM v5: периодическая чистка done (через ProjectRetention) и cancelled
	// (через ProjectCancelledRetention) проектов.
	go runOldProjectsCleanupTicker(rootCtx, projectsSvc,
		cfg.ProjectRetention, cfg.ProjectCancelledRetention, cfg.ProjectCleanupInterval, logger)

	// S3 orphan sweep: presigned uploads/удалённые портфолио оставляют
	// «осиротевшие» объекты в bucket'е. Раз в S3SweepInterval листим
	// portfolio/ и images/, удаляем не-referenced + старше S3OrphanMinAge.
	// Поднимается только если есть s3-credentials, иначе no-op.
	if cfg.S3AccessKey != "" && cfg.S3SecretKey != "" {
		s3Client, err := s3.New(s3.Config{
			Endpoint:  cfg.S3Endpoint,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
			Bucket:    cfg.S3Bucket,
			Region:    cfg.S3Region,
			UseSSL:    cfg.S3UseSSL,
			PublicURL: cfg.S3PublicURL,
		})
		if err != nil {
			slog.Warn("worker: s3 sweep disabled", "err", err)
		} else {
			profilesSvc := profiles.NewService(profiles.NewRepo(pool)).WithMediaStorage(s3Client)
			go runMediaSweepTicker(rootCtx, profilesSvc, cfg.S3OrphanMinAge, cfg.S3SweepInterval, logger)
			slog.Info("worker: s3 sweep ready",
				"bucket", s3Client.Bucket(),
				"interval", cfg.S3SweepInterval,
				"min_age", cfg.S3OrphanMinAge)
		}
	} else {
		slog.Info("worker: s3 sweep disabled (no credentials)")
	}

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

// runOldProjectsCleanupTicker — периодически удаляет done-проекты старше
// doneRetention и cancelled-проекты старше cancelledRetention.
func runOldProjectsCleanupTicker(ctx context.Context, svc *projects.Service, doneRetention, cancelledRetention, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 || (doneRetention <= 0 && cancelledRetention <= 0) {
		return
	}
	if n, err := svc.RunOldProjectsCleanup(ctx, doneRetention, cancelledRetention); err != nil {
		logger.Warn("old projects cleanup initial run failed", "err", err)
	} else if n > 0 {
		logger.Info("old projects cleaned up", "count", n)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := svc.RunOldProjectsCleanup(ctx, doneRetention, cancelledRetention)
			if err != nil {
				logger.Warn("old projects cleanup failed", "err", err)
				continue
			}
			if n > 0 {
				logger.Info("old projects cleaned up", "count", n)
			}
		}
	}
}

// runMediaSweepTicker — периодически удаляет orphan-объекты из S3
// (presigned uploads без записи в БД, либо файлы от удалённых портфолио/
// аватаров). interval<=0 → выключено.
func runMediaSweepTicker(ctx context.Context, svc *profiles.Service, minAge, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		return
	}
	// Первый прогон сразу — чтобы при рестарте worker'а долго не ждать.
	if deleted, kept, err := svc.SweepOrphanMedia(ctx, minAge); err != nil {
		logger.Warn("s3 sweep initial run failed", "err", err)
	} else if deleted > 0 {
		logger.Info("s3 sweep", "deleted", deleted, "kept", kept)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			deleted, kept, err := svc.SweepOrphanMedia(ctx, minAge)
			if err != nil {
				logger.Warn("s3 sweep failed", "err", err)
				continue
			}
			if deleted > 0 {
				logger.Info("s3 sweep", "deleted", deleted, "kept", kept)
			}
		}
	}
}

// runReviewAutoSkipTicker — периодически (interval) скипает просроченные
// review-шаги. На пустой выборке — no-op (запрос дешёвый через частичный
// индекс). При ошибке сканирования логируем и идём дальше; per-шаговые
// ошибки уже учтены внутри Service.RunReviewAutoSkip (failed-счётчик).
func runReviewAutoSkipTicker(ctx context.Context, svc *projects.Service, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = time.Hour
	}
	// первый прогон сразу, чтобы при рестарте worker'а не ждать interval.
	if skipped, failed, err := svc.RunReviewAutoSkip(ctx); err != nil {
		logger.Warn("review auto-skip initial run failed", "err", err)
	} else if skipped > 0 || failed > 0 {
		logger.Info("review auto-skip", "skipped", skipped, "failed", failed)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			skipped, failed, err := svc.RunReviewAutoSkip(ctx)
			if err != nil {
				logger.Warn("review auto-skip failed", "err", err)
				continue
			}
			if skipped > 0 || failed > 0 {
				logger.Info("review auto-skip", "skipped", skipped, "failed", failed)
			}
		}
	}
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
