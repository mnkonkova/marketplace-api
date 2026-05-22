package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"marketpclce/internal/config"
	"marketpclce/internal/mailer"
	"marketpclce/internal/outbox"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
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
	if err := esClient.EnsureIndex(rootCtx, cfg.OpenSearchIndexProfile, search.IndexMapping()); err != nil {
		slog.Error("ensure index", "err", err)
		os.Exit(1)
	}
	if err := esClient.EnsureIndex(rootCtx, cfg.OpenSearchIndexFeedVideos, search.FeedVideoMapping()); err != nil {
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
		default:
			slog.Warn("unknown email event", "type", eventType)
			return nil
		}
	}

	worker := outbox.NewWorker(pool, logger,
		map[string]outbox.Handler{
			outbox.AggregateSpecialist: specialistHandler,
			outbox.AggregateEmail:      emailHandler,
		},
		outbox.Config{})

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
