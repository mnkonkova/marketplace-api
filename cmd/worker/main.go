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

	indexer := search.NewIndexer(search.NewRepo(pool), esClient, cfg.OpenSearchIndexProfile)

	specialistHandler := func(ctx context.Context, aggregateID, eventType string, _ []byte) error {
		uid, err := uuid.Parse(aggregateID)
		if err != nil {
			return err
		}
		switch eventType {
		case outbox.EventSpecialistDeleted:
			return indexer.Delete(ctx, uid)
		default:
			return indexer.Reconcile(ctx, uid)
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
