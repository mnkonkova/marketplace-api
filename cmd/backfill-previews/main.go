// cmd/backfill-previews — однократная команда: эмитит outbox-событие
// portfolio.video_uploaded для всех portfolio_items с preview_status='pending'.
//
// Когда нужно:
//   - после миграции 00020 на проде (существующие видео все pending,
//     событий в outbox нет — preview никогда не сгенерируется).
//   - после reset'a S3-бакета или для перегенерации после фикса pipeline'a
//     (предварительно UPDATE preview_status='pending' WHERE preview_url IS NULL).
//
// Idempotency: запускать можно сколько угодно — handler'у в worker'е
// возвращает success-skip если запись уже processing/ready (claim
// возвращает RowsAffected=0). Дубли outbox-событий безвредны.
//
// Запуск (на проде):
//   docker compose -f docker-compose.prod.yml --env-file .env.prod \
//       run --rm worker backfill-previews
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"marketpclce/internal/config"
	"marketpclce/internal/outbox"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/s3"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Нужен только PublicURL → KeyFromURL для резолва ключа из video_url.
	// Транскод запускает worker сам, через outbox.
	s3Client, err := s3.New(s3.Config{
		Endpoint:  cfg.S3Endpoint,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
		Bucket:    cfg.S3Bucket,
		Region:    cfg.S3Region,
		UseSSL:    cfg.S3UseSSL,
		PublicURL: cfg.S3PublicURL,
		CDNBaseURL: cfg.CDNBaseURL,
	})
	if err != nil {
		slog.Error("s3 client init failed", "err", err)
		os.Exit(1)
	}

	enqueued, skipped, errCount := run(ctx, pool, s3Client)
	slog.Info("backfill-previews done",
		"enqueued", enqueued,
		"skipped_external_url", skipped,
		"errors", errCount,
	)
	if errCount > 0 {
		os.Exit(1)
	}
}

// keyResolver — узкий контракт для тестируемости (s3.Client тоже его реализует).
type keyResolver interface {
	KeyFromURL(rawURL string) string
}

func run(ctx context.Context, pool *pgxpool.Pool, keys keyResolver) (enqueued, skipped, errs int) {
	rows, err := pool.Query(ctx, `
SELECT id, user_id, COALESCE(video_url, '')
FROM portfolio_items
WHERE preview_status = 'pending'
  AND video_url IS NOT NULL
  AND video_url <> ''
ORDER BY created_at ASC`)
	if err != nil {
		slog.Error("query pending", "err", err)
		return 0, 0, 1
	}
	defer rows.Close()

	type job struct {
		ItemID   string
		UserID   string
		VideoURL string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ItemID, &j.UserID, &j.VideoURL); err != nil {
			slog.Error("scan pending", "err", err)
			return enqueued, skipped, errs + 1
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		slog.Error("iterate pending", "err", err)
		return enqueued, skipped, errs + 1
	}
	slog.Info("found pending items", "count", len(jobs))

	for _, j := range jobs {
		key := keys.KeyFromURL(j.VideoURL)
		if key == "" {
			// Внешний URL (загружен до D6, который теперь блокирует
			// non-bucket URLs). Транскодить нечего — оставляем pending,
			// фронт продолжает показывать оригинал.
			slog.Warn("skip: external video_url, no s3 key",
				"item_id", j.ItemID, "video_url", j.VideoURL)
			skipped++
			continue
		}
		if err := emit(ctx, pool, j.ItemID, j.UserID, key); err != nil {
			slog.Error("emit failed", "item_id", j.ItemID, "err", err)
			errs++
			continue
		}
		enqueued++
	}
	return enqueued, skipped, errs
}

func emit(ctx context.Context, pool *pgxpool.Pool, itemID, userID, key string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := outbox.Emit(ctx, tx, outbox.AggregatePortfolio, itemID,
		outbox.EventPortfolioVideoUploaded, outbox.PortfolioVideoUploadedPayload{
			ItemID: itemID,
			UserID: userID,
			S3Key:  key,
		}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
