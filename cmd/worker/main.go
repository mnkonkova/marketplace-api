package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"marketpclce/internal/billing"
	"marketpclce/internal/config"
	"marketpclce/internal/eventroute"
	"marketpclce/internal/instacurl"
	"marketpclce/internal/notifications"
	"marketpclce/internal/orders"
	"marketpclce/internal/outbox"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/profiles"
	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
	"marketpclce/internal/search"
	"marketpclce/internal/transcode"
)

// dispatcherOrNil — nil-указатель, положенный в интерфейс, перестаёт
// быть nil: проверка `d.CRM == nil` в обработчике его не поймала бы, и
// «адрес не задан» превратилось бы в вызов метода на nil-приёмнике.
// Send такое переживает, но полагаться на это нельзя — интерфейс должен
// быть честно пустым.
func dispatcherOrNil(d *notifications.WebhookDispatcher) eventroute.Dispatcher {
	if d == nil {
		return nil
	}
	return d
}

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

	// Reindex-on-start: при смене analyzer'a / маппинга (например, добавили
	// synonym_graph) старый индекс продолжает использовать старую конфигурацию,
	// потому что settings.analysis нельзя менять in-place. Флаг заставляет
	// снести оба индекса и создать заново — worker потом сам сделает bootstrap.
	// Даунтайм поиска на время recreate + bootstrap (~5-30 сек в MVP-объёме).
	//
	// Правило деплоя: включить в .env.prod=true ночью → задеплоить → дождаться
	// «bootstrapped» лога → вернуть в false в env → редеплой worker'a.
	// См. docs/DEPLOY.md → «Reindex на смене маппинга».
	if cfg.OpenSearchReindexOnStart {
		slog.Warn("OPENSEARCH_REindex_ON_START=true — DROP both indices, will rebuild from scratch")
		if err := esClient.DeleteIndex(rootCtx, cfg.OpenSearchIndexProfile); err != nil {
			slog.Error("drop specialists index", "err", err)
			os.Exit(1)
		}
		if err := esClient.DeleteIndex(rootCtx, cfg.OpenSearchIndexFeedVideos); err != nil {
			slog.Error("drop feed_videos index", "err", err)
			os.Exit(1)
		}
	}

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

	// Bootstrap: если feed_videos пустой (первый запуск после деплоя Stage 2,
	// после ручного reset'а или после OPENSEARCH_REINDEX_ON_START) — прогоняем
	// всех опубликованных спецов один раз. Дальше держим индекс актуальным
	// через outbox-события.
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

	// Bootstrap для specialists-индекса: при recreate он тоже пустой и надо
	// прогнать всех published-approved спецов через Reconcile. Раньше это
	// делалось только для feed_videos — при redirect-фикс'ах пришли к тому
	// что тоже нужно для specialists (иначе SearchResultsPage пустой).
	if err := bootstrapSpecialistsIfEmpty(rootCtx, esClient, cfg.OpenSearchIndexProfile, indexer, repo); err != nil {
		slog.Warn("specialists bootstrap skipped", "err", err)
	}

	// email.* (verify_send, password_reset_send) ходят только через n8n
	// (workflow crmEmailNotify, см. deploy/n8n/workflows/). n8n получает
	// {event_id, event_type, data:{to,token,base_url}}, сам рендерит HTML
	// и шлёт через UniSender (или любой SMTP/HTTP-провайдер из своего UI).
	// Если N8N_EMAIL_WEBHOOK_URL пустой — событие логируется в stdout
	// (verify-URL копируется руками; типично для локального запуска без n8n)
	// и квитируется, чтобы не висеть в outbox-ретраях.
	n8nEmailDispatcher := notifications.NewWebhookDispatcher(cfg.N8nEmailWebhookURL, cfg.N8nWebhookToken, cfg.AppBaseURL)
	if n8nEmailDispatcher == nil {
		slog.Info("n8n email webhook disabled (N8N_EMAIL_WEBHOOK_URL empty) — verify-URL будет печататься в stdout")
	} else {
		slog.Info("n8n email webhook ready", "url", cfg.N8nEmailWebhookURL)
	}

	// n8n-диспатчер для CRM-событий project.*. nil = выключен (события
	// квитируются как no-op, чтобы не зависать в outbox-ретраях).
	n8nDispatcher := notifications.NewWebhookDispatcher(cfg.N8nWebhookURL, cfg.N8nWebhookToken, cfg.AppBaseURL)
	if n8nDispatcher == nil {
		slog.Info("n8n webhook disabled (N8N_WEBHOOK_URL empty) — project.* events будут no-op")
	} else {
		slog.Info("n8n webhook ready", "url", cfg.N8nWebhookURL)
	}

	// support.message_received → отдельный workflow (дамп в Telegram).
	n8nSupportDispatcher := notifications.NewWebhookDispatcher(cfg.N8nSupportWebhookURL, cfg.N8nWebhookToken, cfg.AppBaseURL)
	if n8nSupportDispatcher == nil {
		slog.Info("n8n support webhook disabled (N8N_SUPPORT_WEBHOOK_URL empty)")
	} else {
		slog.Info("n8n support webhook ready", "url", cfg.N8nSupportWebhookURL)
	}
	// portfolio.video_uploaded → транскодинг preview (480p, ~500KB) через
	// локальный ffmpeg. См. docs/VIDEO_TRANSCODING.md.
	// Условия для активации:
	//   1) ffmpeg есть в PATH (или указан через FFMPEG_PATH)
	//   2) S3-ключи сконфигурены (нужен Get+Put на bucket)
	// Если что-то из этого нет — handler стартует как no-op (логирует и
	// квитирует событие), worker не валится. Это позволяет запускать
	// воркер локально без ffmpeg/без S3 и видеть остальные хендлеры.
	var transcoder eventroute.Transcoder
	ffmpeg, ffmpegErr := transcode.NewFFmpegBin(cfg.FFmpegPath, cfg.TranscodeTimeout)
	switch {
	case ffmpegErr != nil:
		slog.Warn("transcode disabled (ffmpeg not available) — portfolio.video_uploaded acked as no-op",
			"err", ffmpegErr)
	case cfg.S3AccessKey == "" || cfg.S3SecretKey == "":
		slog.Warn("transcode disabled (S3 creds not set) — portfolio.video_uploaded acked as no-op")
	default:
		s3TC, err := s3.New(s3.Config{
			Endpoint:   cfg.S3Endpoint,
			AccessKey:  cfg.S3AccessKey,
			SecretKey:  cfg.S3SecretKey,
			Bucket:     cfg.S3Bucket,
			Region:     cfg.S3Region,
			UseSSL:     cfg.S3UseSSL,
			PublicURL:  cfg.S3PublicURL,
			CDNBaseURL: cfg.CDNBaseURL,
		})
		if err != nil {
			slog.Error("transcode s3 client init failed", "err", err)
			os.Exit(1)
		}
		svc, err := transcode.NewService(transcode.Config{
			FFmpeg:  ffmpeg,
			Storage: s3TC,
			TempDir: cfg.TranscodeTempDir,
			DB:      pool,
		}, logger)
		if err != nil {
			slog.Error("transcode service init failed", "err", err)
			os.Exit(1)
		}
		transcoder = svc
		slog.Info("transcode ready", "ffmpeg_timeout", cfg.TranscodeTimeout, "tempdir", cfg.TranscodeTempDir)
	}

	// Маршрутизация событий живёт в internal/eventroute: сами
	// обработчики и таблица «агрегат → обработчик» там же, здесь только
	// сборка зависимостей. Замыкания посреди main() нельзя было позвать
	// из теста, и перепутанные местами вебхуки никто бы не заметил.
	worker := outbox.NewWorker(pool, logger,
		eventroute.Handlers(eventroute.Deps{
			CRM:        dispatcherOrNil(n8nDispatcher),
			Email:      dispatcherOrNil(n8nEmailDispatcher),
			Support:    dispatcherOrNil(n8nSupportDispatcher),
			Search:     indexer,
			Feed:       feedIndexer,
			Transcoder: transcoder,
			Logger:     logger,
		}),
		outbox.Config{
			MaxAttempts:     cfg.OutboxMaxAttempts,
			BackoffCap:      cfg.OutboxBackoffCap,
			Retention:       cfg.OutboxRetention,
			DeadRetention:   cfg.OutboxDeadRetention,
			CleanupInterval: cfg.OutboxCleanupInterval,
		})

	// CRM v5 Ф5: периодический таск авто-skip просроченных review-шагов.
	// Длительность дедлайна берётся из cfg.ReviewDeadline (по умолчанию 7
	// дней); интервал сканирования — cfg.ReviewCheckInterval (1 час).
	projectsRepo := projects.NewRepo(pool)
	projectsSvc := projects.NewService(projectsRepo).
		WithReviewDeadline(cfg.ReviewDeadline)
	go runReviewAutoSkipTicker(rootCtx, projectsSvc, cfg.ReviewCheckInterval, logger)
	go runPublicationRemindersTicker(rootCtx,
		publications.NewService(publications.NewRepo(pool)),
		cfg.PublicationRemindersInterval, cfg.PublicationRemindersAfterHour, logger)

	go runOrderExpiryTicker(rootCtx, orders.NewService(orders.NewRepo(pool)),
		cfg.OrderExpiryInterval, logger)

	// Подытог периодов. До него правило «через две недели после конца
	// периода просмотры больше не меняются» не исполнялось нигде: оно
	// держалось на том, что менеджер вовремя нажал «Пересчитать».
	go runPeriodLockTicker(rootCtx, billing.NewService(billing.NewRepo(pool)),
		cfg.BillingPeriodLockInterval, cfg.BillingPeriodLockDelay, logger)

	go runPublicationGaugeTicker(rootCtx,
		publications.NewService(publications.NewRepo(pool)), 5*time.Minute, logger)

	// Сбор статистики. Клиент nil, если адрес или ключ не заданы — тогда
	// тикер не поднимается и в логе один внятный warn вместо ежечасных
	// ошибок.
	if ic := instacurl.New(cfg.InstacurlURL, cfg.InstacurlAPIKey, cfg.InstacurlTimeout); ic != nil {
		go runStatsCollectTicker(rootCtx,
			publications.NewService(publications.NewRepo(pool)).WithCollector(ic),
			cfg.StatsCollectInterval, cfg.StatsCollectBatch,
			cfg.StatsCollectBatchesPerTick, logger)
	} else {
		logger.Warn("stats collection disabled: INSTACURL_URL or INSTACURL_API_KEY not set")
	}
	// S3 orphan sweep: presigned uploads/удалённые портфолио оставляют
	// «осиротевшие» объекты в bucket'е. Раз в S3SweepInterval листим
	// portfolio/ и images/, удаляем не-referenced + старше S3OrphanMinAge.
	//
	// Используется ОТДЕЛЬНАЯ пара S3_SWEEP_* ключей: основной S3_ACCESS_KEY
	// у фронта upload-only (presigned PUT), без list/delete; для sweep'a
	// нужен сервис-аккаунт с list+delete. Если sweep-ключи пусты — sweep
	// выключен (no-op), фронт-аплоад продолжает работать.
	if cfg.S3SweepAccessKey != "" && cfg.S3SweepSecretKey != "" {
		s3Client, err := s3.New(s3.Config{
			Endpoint:   cfg.S3Endpoint,
			AccessKey:  cfg.S3SweepAccessKey,
			SecretKey:  cfg.S3SweepSecretKey,
			Bucket:     cfg.S3Bucket,
			Region:     cfg.S3Region,
			UseSSL:     cfg.S3UseSSL,
			PublicURL:  cfg.S3PublicURL,
			CDNBaseURL: cfg.CDNBaseURL,
		})
		if err != nil {
			slog.Warn("worker: s3 sweep disabled", "err", err)
		} else {
			profilesSvc := profiles.NewService(profiles.NewRepo(pool)).WithMediaStorage(profiles.NewS3MediaStorage(s3Client))
			go runMediaSweepTicker(rootCtx, profilesSvc, cfg.S3OrphanMinAge, cfg.S3SweepInterval, logger)
			slog.Info("worker: s3 sweep ready",
				"bucket", s3Client.Bucket(),
				"interval", cfg.S3SweepInterval,
				"min_age", cfg.S3OrphanMinAge)
		}
	} else {
		slog.Info("worker: s3 sweep disabled (S3_SWEEP_* keys not set)")
	}

	// transcode_queue_depth gauge — обновляется раз в 30 сек тем же
	// циклом что и outbox-gauges. Партиальный индекс
	// portfolio_items_pending_preview_idx делает count дешёвым.
	go runTranscodeGaugeTicker(rootCtx, pool, 30*time.Second, logger)
	go runBusinessGaugeTicker(rootCtx, pool, 5*time.Minute, logger)

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

// runBusinessGaugeTicker — фоновое обновление бизнес-gauge'ов проектов/
// лидов/комментариев/юзеров. Интервал 5 минут — достаточно для дашборда,
// нагрузка минимальна (~7 SELECT с индексами). interval<=0 → выкл.
func runBusinessGaugeTicker(ctx context.Context, db *pgxpool.Pool, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		return
	}
	if err := projects.RefreshBusinessGauges(ctx, db); err != nil {
		logger.Warn("business gauge initial refresh", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := projects.RefreshBusinessGauges(ctx, db); err != nil {
				logger.Warn("business gauge refresh", "err", err)
			}
		}
	}
}

// runTranscodeGaugeTicker — фоновое обновление transcode_queue_depth.
// Аналог outbox.refreshGauges, но для очереди преvью. interval<=0 → выкл.
func runTranscodeGaugeTicker(ctx context.Context, db *pgxpool.Pool, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		return
	}
	if err := transcode.RefreshQueueDepth(ctx, db); err != nil {
		logger.Warn("transcode gauge initial refresh", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := transcode.RefreshQueueDepth(ctx, db); err != nil {
				logger.Warn("transcode gauge refresh", "err", err)
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

// runPublicationRemindersTicker — напоминания креаторам о выкладках и
// сводка менеджерам в общий чат.
//
// Тик частый, а рассылка — раз в день: повтор гасится уникальным индексом
// в notification_log, а не памятью процесса. Поэтому рестарт воркера
// безопасен, а после простоя напоминания уходят при первом же тике.
func runPublicationRemindersTicker(ctx context.Context, svc *publications.Service,
	interval time.Duration, afterHour int, logger *slog.Logger) {

	if interval <= 0 {
		interval = time.Hour
	}
	if afterHour < 0 || afterHour > 23 {
		afterHour = publications.DefaultReminderHour
	}

	run := func() {
		now := time.Now()
		if !publications.ReminderWindowOpen(now, afterHour) {
			return
		}
		st, err := svc.RunReminders(ctx, now)
		if err != nil {
			logger.Warn("publication reminders failed", "err", err)
			return
		}
		if st.Sent > 0 || st.Failures > 0 || st.Digests > 0 {
			logger.Info("publication reminders",
				"considered", st.Considered, "sent", st.Sent,
				"skipped", st.Skipped, "digests", st.Digests, "failures", st.Failures)
		}
	}

	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// runOrderExpiryTicker — сгорание приглашений и пинги менеджеру.
//
// Приглашение живёт трое суток, поэтому частый тик не нужен; но и редкий
// плох: место освободится, а следующего позовут только на следующем
// проходе, и клиент будет ждать зря. Час — разумная середина.
func runOrderExpiryTicker(ctx context.Context, svc *orders.Service,
	interval time.Duration, logger *slog.Logger) {

	if interval <= 0 {
		interval = time.Hour
	}
	run := func() {
		expired, pinged, err := svc.RunExpiry(ctx, time.Now())
		if err != nil {
			logger.Warn("order expiry failed", "err", err)
			return
		}
		if expired > 0 || pinged > 0 {
			logger.Info("order expiry", "expired", expired, "manager_pinged", pinged)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// runPeriodLockTicker — подытоживает периоды, которым пора.
//
// Период закрывается через delay после своего конца: просмотры
// сохраняются срезом на отсечку, суммы пересчитываются по нему, в общий
// чат уходит сообщение. Дальше числа периода не меняются, даже если
// ролики продолжают набирать просмотры, — в этом и смысл.
//
// Ошибка на одном периоде не роняет проход: остальные проекты в ней не
// виноваты, а следующий тик попробует снова.
func runPeriodLockTicker(ctx context.Context, svc *billing.Service,
	interval, delay time.Duration, logger *slog.Logger) {

	if interval <= 0 {
		interval = time.Hour
	}
	run := func() {
		locked, failed, err := svc.LockDuePeriods(ctx, time.Now().UTC(), delay)
		if err != nil {
			logger.Warn("period lock scan failed", "err", err)
			return
		}
		if locked > 0 || failed > 0 {
			logger.Info("periods locked", "locked", locked, "failed", failed)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// runPublicationGaugeTicker — обновление бизнес-gauge'ов проектной
// страницы: просроченные выкладки, отставание сбора, застрявшие ссылки.
//
// Отдельно от сбора: gauge'и должны обновляться, даже когда интеграция с
// instacurl не настроена — иначе «сбор выключен» и «сбор сломан» выглядят
// на дашборде одинаково.
func runPublicationGaugeTicker(ctx context.Context, svc *publications.Service,
	interval time.Duration, logger *slog.Logger) {

	if interval <= 0 {
		interval = 5 * time.Minute
	}
	run := func() {
		if _, err := svc.RefreshGauges(ctx, time.Now()); err != nil {
			logger.Warn("publication gauges refresh failed", "err", err)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// runStatsCollectTicker — ежедневный обход сданных роликов и схлопывание
// рядов по закрытым проектам.
//
// Тик может быть частым: правило «один ролик не чаще раза в сутки» и
// график 1→2→4→8 держатся в выборке DueForCollection, а не в периоде
// тикера. Поэтому рестарт воркера не приводит к повторному обходу и не
// жжёт кредиты.
func runStatsCollectTicker(ctx context.Context, svc *publications.Service,
	interval time.Duration, batch, batchesPerTick int, logger *slog.Logger) {

	if interval <= 0 {
		interval = time.Hour
	}
	if batch <= 0 {
		batch = 10
	}
	if batchesPerTick <= 0 {
		batchesPerTick = 20
	}

	run := func() {
		now := time.Now()
		// Пачка мелкая (см. StatsCollectBatch), поэтому за тик прогоняем
		// несколько подряд — пока есть что собирать. Иначе часовой тик
		// упирался бы в десять ссылок и не успевал за объёмом.
		var total publications.CollectStats
		for i := 0; i < batchesPerTick; i++ {
			st, err := svc.RunCollection(ctx, time.Now(), batch)
			total.Considered += st.Considered
			total.Saved += st.Saved
			total.NoData += st.NoData
			total.ThresholdNotified += st.ThresholdNotified
			if err != nil {
				logger.Warn("stats collection failed", "err", err,
					"batch", i+1, "collected_before_failure", total.Saved)
				break
			}
			// Собирать больше нечего — ждём следующего тика.
			if st.Considered == 0 {
				break
			}
			if ctx.Err() != nil {
				return
			}
		}
		if total.Considered > 0 {
			logger.Info("stats collection",
				"considered", total.Considered, "saved", total.Saved,
				"no_data", total.NoData, "threshold_notified", total.ThresholdNotified)
		}
		// Схлопывание идёт следом: проект, у которого вышел срок сбора,
		// на этом же проходе перестаёт опрашиваться.
		if n, err := svc.RunCollapse(ctx, now); err != nil {
			logger.Warn("stats collapse failed", "err", err)
		} else if n > 0 {
			logger.Info("stats collapsed", "projects", n)
		}
	}

	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ensureIndexWithRetry — EnsureIndex с экспоненциальным ожиданием.
// На холодном старте docker compose OpenSearch ещё может не принимать
// соединения; раньше worker падал с os.Exit(1) и outbox-события копились
// без обработчика. 60 секунд (60×1 c) с запасом покрывают cold start.
// bootstrapSpecialistsIfEmpty — если specialists-индекс пуст, прогоняет
// Reconcile для всех published-approved спецов из PG. Симметрично bootstrap'у
// feed_videos: нужен после reindex-on-start и при первом деплое.
//
// version=0 в Reconcile — фолбэк на безверсионный upsert (indexer.go
// сам разрулит). Ошибки одного спеца логируем но не валим весь bootstrap.
func bootstrapSpecialistsIfEmpty(ctx context.Context, esClient *es.Client, index string, indexer *search.Indexer, repo *search.Repo) error {
	n, err := esClient.CountDocs(ctx, index)
	if err != nil {
		return fmt.Errorf("count specialists: %w", err)
	}
	if n > 0 {
		return nil // индекс уже наполнен, не трогаем
	}
	ids, err := repo.LoadPublishedSpecialistIDs(ctx)
	if err != nil {
		return fmt.Errorf("load ids: %w", err)
	}
	success := 0
	for _, id := range ids {
		if err := indexer.Reconcile(ctx, id, 0); err != nil {
			slog.Warn("specialists bootstrap reconcile failed", "user_id", id, "err", err)
			continue
		}
		success++
	}
	slog.Info("specialists bootstrapped", "total", len(ids), "indexed", success)
	return nil
}

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
