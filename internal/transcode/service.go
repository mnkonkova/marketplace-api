// Package transcode — async pipeline для генерации preview-видео
// (480p, 5-10 сек) к загруженным в портфолио оригиналам. Подробности
// архитектуры — docs/VIDEO_TRANSCODING.md.
package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
)

// FFmpeg — обёртка над ffmpeg-бинарём. Service вызывает ровно один
// метод; реализация может быть реальной (cmd.Exec) или моком в тестах.
type FFmpeg interface {
	// MakePreview читает input, пишет preview по output. Возвращает
	// ErrPermanent если файл битый/невалидный — handler пробросит дальше
	// в outbox.ErrPermanent, чтобы запись сразу попала в DLQ без retry.
	MakePreview(ctx context.Context, input, output string) error

	// MakeAnimatedWebP — animated WebP «гифка» (autoplay через <img> на
	// главной, без iOS Low Power Mode блокировок). См. §11 docs.
	MakeAnimatedWebP(ctx context.Context, input, output string, p GifParams) error

	// ProbeDuration — длительность через ffprobe (для выбора gif-params).
	// 0, err = caller использует дефолтный средний бакет.
	ProbeDuration(ctx context.Context, input string) (float64, error)
}

// Storage — подмножество S3-клиента, нужное pipeline'у. Реализуется
// `*s3.Client` (но не привязываемся через имена пакетов).
type Storage interface {
	// Download качает объект по ключу в локальный файл. Возвращает
	// ErrNotFound если ключа нет — handler переводит в permanent.
	Download(ctx context.Context, key, localPath string) error
	// Upload заливает локальный файл в S3 по ключу с указанным
	// contentType (для preview всегда "video/mp4").
	Upload(ctx context.Context, key, localPath, contentType string) error
	// PublicURL возвращает публичный URL для ключа (без presign — preview
	// всегда public-read через bucket policy).
	PublicURL(key string) string
}

// ErrPermanent — pipeline считает событие неретраемым (битый файл /
// удалённый оригинал / превышен размер). Handler оборачивает в
// outbox.ErrPermanent чтобы worker отправил в DLQ сразу.
var ErrPermanent = errors.New("transcode: permanent failure")

// ErrSkipped — handler счёл что preview уже не нужен (запись удалена,
// или preview уже ready). Это success-эквивалент, событие отмечается
// processed, без retry.
var ErrSkipped = errors.New("transcode: skipped")

type Config struct {
	FFmpeg  FFmpeg
	Storage Storage
	TempDir string // /tmp/transcode по дефолту, создаётся если нет
	DB      *pgxpool.Pool
	// StuckRecoveryAfter — auto-recovery от записей залипших в
	// preview_status='processing' (worker крашнулся между claim и
	// markReady/markFailed, lease на outbox-событие выпал, но статус
	// строки остался processing). Если за это время записи не дождались
	// markReady, повторный claim переводит их обратно в processing и
	// гонит pipeline заново. Должно быть ≥ outbox leaseDuration (10 мин),
	// иначе два воркера могут одновременно процессить. Default 15 мин.
	StuckRecoveryAfter time.Duration
}

type Service struct {
	cfg    Config
	logger *slog.Logger
}

func NewService(cfg Config, logger *slog.Logger) (*Service, error) {
	if cfg.FFmpeg == nil {
		return nil, fmt.Errorf("transcode: FFmpeg is required")
	}
	if cfg.Storage == nil {
		return nil, fmt.Errorf("transcode: Storage is required")
	}
	if cfg.DB == nil {
		return nil, fmt.Errorf("transcode: DB is required")
	}
	if cfg.TempDir == "" {
		cfg.TempDir = "/tmp/transcode"
	}
	if cfg.StuckRecoveryAfter <= 0 {
		cfg.StuckRecoveryAfter = 15 * time.Minute
	}
	if err := os.MkdirAll(cfg.TempDir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: mkdir tempdir: %w", err)
	}
	// Startup cleanup: при крэше предыдущего инстанса в tempdir могли
	// остаться .mp4-сироты (download был, но defer не успел). Чистим их
	// чтобы диск не утекал. ReadDir по dir в /tmp дешёвый.
	if entries, err := os.ReadDir(cfg.TempDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				_ = os.Remove(filepath.Join(cfg.TempDir, e.Name()))
			}
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{cfg: cfg, logger: logger}, nil
}

// Process — обработчик outbox-события portfolio.video_uploaded. Гонит
// пайплайн: claim row → download → ffmpeg → upload → mark ready.
//
// Идемпотентность: первый UPDATE пытается перевести строку из
// pending/failed в processing. Если RowsAffected=0 — кто-то уже обработал
// (другой воркер, мануальный запуск backfill'a), возвращаем nil (skip).
//
// На любой ErrPermanent помечаем preview_status='failed' с описанием
// ошибки. Транзиентные ошибки оставляют статус processing, lease воркера
// (10 мин, P3) автоматически освободит запись для повторной попытки.
func (s *Service) Process(ctx context.Context, payload []byte) error {
	var p outbox.PortfolioVideoUploadedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("%w: invalid payload: %v", ErrPermanent, err)
	}
	itemID, err := uuid.Parse(p.ItemID)
	if err != nil {
		return fmt.Errorf("%w: bad item_id %q: %v", ErrPermanent, p.ItemID, err)
	}
	userID, err := uuid.Parse(p.UserID)
	if err != nil {
		return fmt.Errorf("%w: bad user_id %q: %v", ErrPermanent, p.UserID, err)
	}
	if p.S3Key == "" {
		return fmt.Errorf("%w: empty s3_key", ErrPermanent)
	}

	// Claim: pending/failed → processing. Race-safe и идемпотентно.
	claimed, err := s.claim(ctx, itemID)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !claimed {
		// Уже ready/processing у кого-то другого — это success-skip.
		s.logger.Info("transcode skipped (already claimed or ready)",
			"item_id", itemID)
		return nil
	}

	previewKey := previewKeyFor(p.S3Key)
	gifKey := animatedThumbKeyFor(p.S3Key)
	inputPath := filepath.Join(s.cfg.TempDir, itemID.String()+".mp4")
	outputPath := filepath.Join(s.cfg.TempDir, itemID.String()+"_preview.mp4")
	gifPath := filepath.Join(s.cfg.TempDir, itemID.String()+"_preview.webp")
	defer func() {
		_ = os.Remove(inputPath)
		_ = os.Remove(outputPath)
		_ = os.Remove(gifPath)
	}()

	start := time.Now()
	if err := s.cfg.Storage.Download(ctx, p.S3Key, inputPath); err != nil {
		return s.handlePipelineErr(ctx, itemID, "download original", err)
	}
	if err := s.cfg.FFmpeg.MakePreview(ctx, inputPath, outputPath); err != nil {
		return s.handlePipelineErr(ctx, itemID, "ffmpeg make preview", err)
	}
	if err := s.cfg.Storage.Upload(ctx, previewKey, outputPath, "video/mp4"); err != nil {
		return s.handlePipelineErr(ctx, itemID, "upload preview", err)
	}
	previewURL := s.cfg.Storage.PublicURL(previewKey)

	// Animated WebP «гифка» (sequential pass): non-critical — если упало,
	// preview_url всё равно доступен, фронт фолбэчит на <video>. См. §11.
	gifURL := s.makeAnimatedThumb(ctx, itemID, inputPath, gifPath, gifKey)

	// T2 fix: markReady + reindex-emit одной tx. Раньше markReady был
	// commit'ом, а emitReindex — отдельной tx после него; crash между
	// ними оставлял preview готовым, но OS feed_videos без preview_url.
	if err := s.commitReady(ctx, itemID, userID, previewURL, gifURL); err != nil {
		return fmt.Errorf("commit ready: %w", err)
	}

	successTotal.Inc()
	durationSeconds.Observe(time.Since(start).Seconds())
	s.logger.Info("transcode complete",
		"item_id", itemID,
		"user_id", userID,
		"preview_url", previewURL,
		"dur", time.Since(start),
	)
	return nil
}

// claim — атомарно перевести запись в processing. true = взяли, false =
// уже занято/готово/удалено. Допускаем:
//   pending  → processing (новая запись)
//   failed   → processing (ручной retry или backfill)
//   processing с updated_at < now - StuckRecoveryAfter → processing
//     (auto-recovery: предыдущий handler крашнулся между claim и markReady,
//     запись осталась залипшей — иначе она навсегда заблокирована).
//
// StuckRecoveryAfter > outbox.leaseDuration (10м), поэтому два воркера
// не могут одновременно claim'нуть один и тот же item.
func (s *Service) claim(ctx context.Context, itemID uuid.UUID) (bool, error) {
	tag, err := s.cfg.DB.Exec(ctx, `
UPDATE portfolio_items
SET preview_status = 'processing',
    preview_error  = NULL,
    updated_at     = now()
WHERE id = $1
  AND (
        preview_status IN ('pending', 'failed')
        OR (preview_status = 'processing' AND updated_at < now() - $2::interval)
      )`, itemID, s.cfg.StuckRecoveryAfter.String())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// commitReady — финальная атомарная операция: ставит preview_url + ready,
// и тем же commit'ом эмитит specialist.upserted для переиндексации
// feed_videos в OS. Защищаемся через WHERE preview_status='processing' от
// перезаписи ручного revert'a админа (Row уехал не туда → markReady no-op,
// outbox event тоже не эмитим, потому что tx сериализован).
//
// Если RowsAffected=0 (row удалили / статус сменили) — outbox event
// всё равно эмитим: feed_videos должен переиндекситься (preview_url
// мог уйти в S3 как orphan, sweep его подберёт).
// makeAnimatedThumb — генерит и заливает animated WebP. Non-critical: если
// упало (ffprobe нет, ffmpeg ошибся, libwebp нет в build'е) — пишем warn
// и возвращаем "", вызывающий код просто не запишет animated_thumb_url.
// Логика выбора параметров — ChooseGifParams(duration). Если ffprobe не
// дал длительность, fallback на 15-сек бакет (середина).
//
// После первого pass'a проверяем размер: > 150KB → второй pass с
// quality-10. Если и второй не вошёл — оставляем что есть (всё равно
// меньше mp4).
func (s *Service) makeAnimatedThumb(ctx context.Context, itemID uuid.UUID, input, output, key string) string {
	dur, err := s.cfg.FFmpeg.ProbeDuration(ctx, input)
	if err != nil {
		s.logger.Warn("animated_thumb: probe failed, using middle bucket",
			"item_id", itemID, "err", err)
		dur = 10 // средний бакет
	}
	params := ChooseGifParams(dur)
	if err := s.cfg.FFmpeg.MakeAnimatedWebP(ctx, input, output, params); err != nil {
		s.logger.Warn("animated_thumb: ffmpeg failed (non-critical, preview_url still ok)",
			"item_id", itemID, "err", err)
		return ""
	}
	info, statErr := os.Stat(output)
	// Sanity-check: ffmpeg может вернуть exit 0 с dummy-файлом (<1KB)
	// на пограничных входах. Не загружаем такой в S3.
	if statErr != nil || info.Size() < 1024 {
		s.logger.Warn("animated_thumb: output too small or missing, skipping upload",
			"item_id", itemID, "size", info.Size())
		return ""
	}
	// Второй pass если > 150KB.
	if info.Size() > 150*1024 {
		params.Quality -= 10
		if params.Quality < 50 {
			params.Quality = 50
		}
		if err := s.cfg.FFmpeg.MakeAnimatedWebP(ctx, input, output, params); err != nil {
			s.logger.Warn("animated_thumb: second pass failed, keeping first",
				"item_id", itemID, "err", err)
		}
	}
	if err := s.cfg.Storage.Upload(ctx, key, output, "image/webp"); err != nil {
		s.logger.Warn("animated_thumb: upload failed",
			"item_id", itemID, "err", err)
		return ""
	}
	return s.cfg.Storage.PublicURL(key)
}

func (s *Service) commitReady(ctx context.Context, itemID, userID uuid.UUID, previewURL, animatedThumbURL string) error {
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
UPDATE portfolio_items
SET preview_url          = $2,
    animated_thumb_url   = NULLIF($3, ''),
    preview_status       = 'ready',
    preview_error        = NULL,
    preview_generated_at = now(),
    updated_at           = now()
WHERE id = $1 AND preview_status = 'processing'`, itemID, previewURL, animatedThumbURL); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
		outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()}); err != nil {
		return fmt.Errorf("emit reindex: %w", err)
	}
	return tx.Commit(ctx)
}

// markFailed — статус failed + сохранение причины. Не валит handler
// (события у нас уже permanent), просто фиксируем для расследований.
func (s *Service) markFailed(ctx context.Context, itemID uuid.UUID, reason string) {
	if _, err := s.cfg.DB.Exec(ctx, `
UPDATE portfolio_items
SET preview_status = 'failed',
    preview_error  = $2,
    updated_at     = now()
WHERE id = $1 AND preview_status = 'processing'`, itemID, reason); err != nil {
		s.logger.Warn("transcode mark failed", "item_id", itemID, "err", err)
	}
}

// handlePipelineErr классифицирует ошибку и опционально помечает
// строку failed. Permanent ошибки идут наружу обёрнутые в ErrPermanent.
func (s *Service) handlePipelineErr(ctx context.Context, itemID uuid.UUID, stage string, err error) error {
	if errors.Is(err, ErrPermanent) {
		s.markFailed(ctx, itemID, stage+": "+err.Error())
		errorsTotal.WithLabelValues("permanent").Inc()
		return fmt.Errorf("%s: %w", stage, err)
	}
	// Транзиентная ошибка: оставляем processing, lease сам откатит запись.
	// Классификация по фразе stage — грубая, но для алертинга достаточно:
	// timeout/network → проблема снаружи, "ffmpeg" → возможно перегруз CPU.
	reason := "other"
	switch stage {
	case "download original", "upload preview":
		reason = "network"
	case "ffmpeg make preview":
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		} else {
			reason = "other"
		}
	}
	errorsTotal.WithLabelValues(reason).Inc()
	return fmt.Errorf("%s: %w", stage, err)
}

// previewKeyFor превращает portfolio/<user>/<item>.mp4 в
// portfolio/<user>/<item>_preview.mp4.
func previewKeyFor(originalKey string) string {
	ext := filepath.Ext(originalKey)
	if ext == "" {
		return originalKey + "_preview.mp4"
	}
	return originalKey[:len(originalKey)-len(ext)] + "_preview" + ext
}

// animatedThumbKeyFor превращает portfolio/<user>/<item>.mp4 в
// portfolio/<user>/<item>_preview.webp (animated WebP «гифка»).
func animatedThumbKeyFor(originalKey string) string {
	ext := filepath.Ext(originalKey)
	if ext == "" {
		return originalKey + "_preview.webp"
	}
	return originalKey[:len(originalKey)-len(ext)] + "_preview.webp"
}
