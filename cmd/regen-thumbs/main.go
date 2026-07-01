// cmd/regen-thumbs — регенерирует poster'ы (thumbnail_url) видео-айтемов.
//
// Зачем: фронт-uploader раньше извлекал кадр на 1.5с — часто попадал в
// чёрный fade-in, юзеры видели чёрные превьюшки в поиске. Теперь фронт
// берёт на 2с (см. portfolio-upload.dialog.ts). Эта команда перегенерит
// старые poster'ы для уже загруженных видео.
//
// Работает: качает video из S3 → ffmpeg -ss 2 -frames:v 1 → jpg → S3 →
// UPDATE portfolio_items.thumbnail_url. Ключ нового thumbnail'а —
// `<video-key-без-расширения>_thumb.jpg` (перезаписывает если есть).
//
// Идемпотентно: повторный запуск даст такой же результат (перезаписывает
// existing thumbnail).
//
// Локальный запуск:
//   go run ./cmd/regen-thumbs --dry-run              # покажет план
//   go run ./cmd/regen-thumbs                        # прогонит всё
//   go run ./cmd/regen-thumbs --user-id=<uuid>       # только одного спеца
//   go run ./cmd/regen-thumbs --limit=5              # первые N
//
// На VDS:
//   docker compose -f docker-compose.prod.yml --env-file .env.prod \
//       run --rm worker regen-thumbs
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"marketpclce/internal/config"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/transcode"
)

const thumbOffsetSec = 2.0

func main() {
	_ = godotenv.Load()

	dryRun := flag.Bool("dry-run", false, "не заливать в S3 и не апдейтить БД, только логировать")
	limit := flag.Int("limit", 0, "лимит обрабатываемых записей (0 = без лимита)")
	userID := flag.String("user-id", "", "обрабатывать только видео этого user_id")
	tempDir := flag.String("temp", os.TempDir(), "директория для промежуточных файлов")
	flag.Parse()

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

	storage, err := s3.New(s3.Config{
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
		slog.Error("s3 init", "err", err)
		os.Exit(1)
	}

	// Таймаут отдельного thumbnail'а: 30с должно хватить (download 10-20MB
	// видео + 1 frame ffmpeg). В dry-run ffmpeg не нужен — команда работает
	// локально без установленного бинаря.
	var ff *transcode.FFmpegBin
	if !*dryRun {
		ff, err = transcode.NewFFmpegBin(cfg.FFmpegPath, 30*time.Second)
		if err != nil {
			slog.Error("ffmpeg init", "err", err)
			os.Exit(1)
		}
	}

	ok, skipped, errCount := run(ctx, pool, storage, ff, *tempDir, *dryRun, *limit, *userID)
	slog.Info("regen-thumbs done",
		"regenerated", ok,
		"skipped", skipped,
		"errors", errCount,
		"dry_run", *dryRun,
	)
	if errCount > 0 {
		os.Exit(1)
	}
}

type job struct {
	itemID   string
	userID   string
	videoURL string
	oldThumb string
}

func run(
	ctx context.Context,
	pool *pgxpool.Pool,
	storage *s3.Client,
	ff *transcode.FFmpegBin,
	tempDir string,
	dryRun bool,
	limit int,
	userIDFilter string,
) (ok, skipped, errs int) {
	query := `
SELECT id::text, user_id::text, COALESCE(video_url, ''), COALESCE(thumbnail_url, '')
FROM portfolio_items
WHERE kind = 'video'
  AND video_url IS NOT NULL AND video_url <> ''
`
	args := []any{}
	if userIDFilter != "" {
		query += ` AND user_id = $1`
		args = append(args, userIDFilter)
	}
	query += ` ORDER BY created_at ASC`
	if limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		slog.Error("query videos", "err", err)
		return 0, 0, 1
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.itemID, &j.userID, &j.videoURL, &j.oldThumb); err != nil {
			slog.Error("scan", "err", err)
			rows.Close()
			return ok, skipped, errs + 1
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("iterate", "err", err)
		return ok, skipped, errs + 1
	}
	slog.Info("plan", "videos", len(jobs), "dry_run", dryRun)

	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			slog.Warn("context cancelled, stopping", "err", err)
			return ok, skipped, errs
		}
		videoKey := storage.KeyFromURL(j.videoURL)
		if videoKey == "" {
			slog.Warn("skip: external video_url (no s3 key)",
				"item_id", j.itemID, "video_url", j.videoURL)
			skipped++
			continue
		}
		thumbKey := thumbKeyFor(videoKey)

		if dryRun {
			slog.Info("would regen", "item_id", j.itemID,
				"video_key", videoKey, "thumb_key", thumbKey, "old_thumb", j.oldThumb)
			ok++
			continue
		}

		if err := regen(ctx, storage, ff, tempDir, j.itemID, videoKey, thumbKey); err != nil {
			slog.Error("regen failed", "item_id", j.itemID, "err", err)
			errs++
			continue
		}
		newURL := storage.PublicURL(thumbKey)
		if err := updateThumb(ctx, pool, j.itemID, newURL); err != nil {
			slog.Error("update thumb_url failed", "item_id", j.itemID, "err", err)
			errs++
			continue
		}
		slog.Info("regenerated", "item_id", j.itemID, "thumb_url", newURL)
		ok++
	}
	return ok, skipped, errs
}

func regen(
	ctx context.Context,
	storage *s3.Client,
	ff *transcode.FFmpegBin,
	tempDir string,
	itemID, videoKey, thumbKey string,
) error {
	inputPath := filepath.Join(tempDir, itemID+".mp4")
	outputPath := filepath.Join(tempDir, itemID+"_thumb.jpg")
	defer func() {
		_ = os.Remove(inputPath)
		_ = os.Remove(outputPath)
	}()

	if err := storage.Download(ctx, videoKey, inputPath); err != nil {
		return fmt.Errorf("download %s: %w", videoKey, err)
	}
	if err := ff.ExtractThumbnail(ctx, inputPath, outputPath, thumbOffsetSec); err != nil {
		// Permanent → пропускаем без ретрая. Транзиентные — тоже пропускаем
		// (регенерация выполняется вручную, не bulk-loop).
		if errors.Is(err, transcode.ErrPermanent) {
			return fmt.Errorf("ffmpeg permanent: %w", err)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	if err := storage.Upload(ctx, thumbKey, outputPath, "image/jpeg"); err != nil {
		return fmt.Errorf("upload %s: %w", thumbKey, err)
	}
	return nil
}

func updateThumb(ctx context.Context, pool *pgxpool.Pool, itemID, url string) error {
	// updated_at не бампаем: регенерация — служебное действие, не должно
	// триггерить optimistic-lock rechecks и outbox-события. thumbnail_url
	// на search-mapping не завязан (см. IndexDoc.PreviewThumbURL — оно
	// денормализуется отдельным LATERAL JOIN и подхватится при следующем
	// reindex'е).
	_, err := pool.Exec(ctx, `
UPDATE portfolio_items SET thumbnail_url = $2 WHERE id = $1`, itemID, url)
	return err
}

// thumbKeyFor — ключ нового thumbnail'а. Кладём рядом с видео с суффиксом
// _thumb.jpg, чтобы:
//   • sweep оркестрован автоматически: удаление видео → удалит и thumb
//     (см. cmd/s3-sweep-once, prefix группировка по user_id/item);
//   • старые thumbnail'ы (если были залиты фронтом отдельным ключом)
//     остаются orphan-объектами, sweep их подберёт в следующем проходе.
func thumbKeyFor(videoKey string) string {
	ext := filepath.Ext(videoKey)
	base := videoKey[:len(videoKey)-len(ext)]
	return base + "_thumb.jpg"
}
