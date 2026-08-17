// cmd/backfill-aspect — проставляет реальные aspect/duration_sec у видео,
// загруженных до появления ffprobe в transcode-пайплайне.
//
// Зачем: колонка aspect существует с миграции 00003, но заполнялась дефолтом
// "9:16" — фронт это поле никогда не присылал, а бэкенд подставлял константу.
// В результате у всех записей, включая горизонтальные, в БД лежит "9:16".
// Публичная страница специалиста рендерит работы в родном формате, поэтому
// врущее поле хуже пустого. Команда качает оригинал, гоняет ffprobe и
// записывает измеренные значения.
//
// Идемпотентно: повторный запуск даёт тот же результат. По умолчанию
// перезаписывает всё (--only-empty оставит уже измеренные строки в покое —
// например, при догоне после частичного прогона).
//
// Локальный запуск:
//
//	go run ./cmd/backfill-aspect --dry-run          # покажет план
//	go run ./cmd/backfill-aspect                    # прогонит всё
//	go run ./cmd/backfill-aspect --user-id=<uuid>   # только одного спеца
//	go run ./cmd/backfill-aspect --limit=5          # первые N
//	go run ./cmd/backfill-aspect --only-empty       # только aspect IS NULL
//
// На VDS:
//
//	docker compose -f docker-compose.prod.yml --env-file .env.prod \
//	    run --rm worker backfill-aspect
package main

import (
	"context"
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
	"marketpclce/internal/mediameta"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/transcode"
)

// probeTimeout — download оригинала (до 30 МБ) + ffprobe. ffprobe читает
// только заголовки, поэтому основное время здесь — сеть.
const probeTimeout = 60 * time.Second

func main() {
	_ = godotenv.Load()

	dryRun := flag.Bool("dry-run", false, "не апдейтить БД, только логировать измеренное")
	limit := flag.Int("limit", 0, "лимит обрабатываемых записей (0 = без лимита)")
	userID := flag.String("user-id", "", "обрабатывать только видео этого user_id")
	onlyEmpty := flag.Bool("only-empty", false, "только записи с пустым aspect")
	tempDir := flag.String("temp", os.TempDir(), "директория для промежуточных файлов")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

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

	// В dry-run ffprobe всё равно нужен — иначе показывать нечего.
	ff, err := transcode.NewFFmpegBin(cfg.FFmpegPath, probeTimeout)
	if err != nil {
		slog.Error("ffmpeg init (нужен ffprobe рядом с ffmpeg)", "err", err)
		os.Exit(1)
	}

	updated, skipped, errCount := run(ctx, pool, storage, ff, *tempDir, *dryRun, *limit, *userID, *onlyEmpty)
	slog.Info("backfill-aspect done",
		"updated", updated,
		"skipped", skipped,
		"errors", errCount,
		"dry_run", *dryRun,
	)
	if errCount > 0 {
		os.Exit(1)
	}
}

type job struct {
	itemID    string
	videoURL  string
	oldAspect string
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
	onlyEmpty bool,
) (updated, skipped, errs int) {
	query := `
SELECT id::text, COALESCE(video_url, ''), COALESCE(aspect, '')
FROM portfolio_items
WHERE kind = 'video'
  AND video_url IS NOT NULL AND video_url <> ''
`
	args := []any{}
	if userIDFilter != "" {
		args = append(args, userIDFilter)
		query += fmt.Sprintf(` AND user_id = $%d`, len(args))
	}
	if onlyEmpty {
		query += ` AND (aspect IS NULL OR aspect = '')`
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
		if err := rows.Scan(&j.itemID, &j.videoURL, &j.oldAspect); err != nil {
			slog.Error("scan", "err", err)
			rows.Close()
			return updated, skipped, errs + 1
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("iterate", "err", err)
		return updated, skipped, errs + 1
	}
	slog.Info("plan", "videos", len(jobs), "dry_run", dryRun, "only_empty", onlyEmpty)

	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			slog.Warn("context cancelled, stopping", "err", err)
			return updated, skipped, errs
		}
		videoKey := storage.KeyFromURL(j.videoURL)
		if videoKey == "" {
			// TODO(backfill): работы со ссылкой на чужой хост так и остаются
			// без aspect — их не трогает ни воркер (нет S3-ключа → нет
			// события portfolio.video_uploaded), ни эта команда. ffprobe
			// умеет читать URL напрямую, но ходить по пользовательскому
			// адресу с нашей инфраструктуры нельзя без фильтра приватных
			// диапазонов. Разбор и план — docs/VIDEO_TRANSCODING.md §12.
			slog.Warn("skip: внешний video_url (нет s3-ключа)",
				"item_id", j.itemID, "video_url", j.videoURL)
			skipped++
			continue
		}

		meta, err := probe(ctx, storage, ff, tempDir, j.itemID, videoKey)
		if err != nil {
			slog.Error("probe failed", "item_id", j.itemID, "err", err)
			errs++
			continue
		}
		aspect := mediameta.AspectFromSize(meta.Width, meta.Height)
		if aspect == "" {
			slog.Warn("skip: ffprobe не дал размеры кадра",
				"item_id", j.itemID, "width", meta.Width, "height", meta.Height)
			skipped++
			continue
		}
		durationSec := int(meta.DurationSec + 0.5)

		if dryRun {
			slog.Info("would update", "item_id", j.itemID,
				"old_aspect", j.oldAspect, "new_aspect", aspect, "duration_sec", durationSec)
			updated++
			continue
		}
		if err := updateMeta(ctx, pool, j.itemID, aspect, durationSec); err != nil {
			slog.Error("update failed", "item_id", j.itemID, "err", err)
			errs++
			continue
		}
		slog.Info("updated", "item_id", j.itemID,
			"old_aspect", j.oldAspect, "new_aspect", aspect, "duration_sec", durationSec)
		updated++
	}
	return updated, skipped, errs
}

func probe(
	ctx context.Context,
	storage *s3.Client,
	ff *transcode.FFmpegBin,
	tempDir string,
	itemID, videoKey string,
) (transcode.VideoMeta, error) {
	inputPath := filepath.Join(tempDir, itemID+"_probe.mp4")
	defer func() { _ = os.Remove(inputPath) }()

	if err := storage.Download(ctx, videoKey, inputPath); err != nil {
		return transcode.VideoMeta{}, fmt.Errorf("download %s: %w", videoKey, err)
	}
	meta, err := ff.Probe(ctx, inputPath)
	if err != nil {
		return transcode.VideoMeta{}, fmt.Errorf("ffprobe: %w", err)
	}
	return meta, nil
}

func updateMeta(ctx context.Context, pool *pgxpool.Pool, itemID, aspect string, durationSec int) error {
	// updated_at не бампаем: backfill — служебное действие, не должно
	// ломать optimistic-lock у открытых вкладок кабинета. duration_sec
	// пишем только если измерили (0 = ffprobe не дал длительность).
	_, err := pool.Exec(ctx, `
UPDATE portfolio_items
   SET aspect       = $2,
       duration_sec = COALESCE(NULLIF($3, 0), duration_sec)
 WHERE id = $1`, itemID, aspect, durationSec)
	return err
}
