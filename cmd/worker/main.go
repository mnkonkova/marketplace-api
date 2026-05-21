package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"marketpclce/internal/config"
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

	worker := outbox.NewWorker(pool, logger,
		map[string]outbox.Handler{outbox.AggregateSpecialist: specialistHandler},
		outbox.Config{})

	if err := worker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("worker stopped", "err", err)
		os.Exit(1)
	}
	slog.Info("worker bye")
}
