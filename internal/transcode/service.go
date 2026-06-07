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
	FFmpeg   FFmpeg
	Storage  Storage
	TempDir  string // /tmp/transcode по дефолту, создаётся если нет
	DB       *pgxpool.Pool
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
	if err := os.MkdirAll(cfg.TempDir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: mkdir tempdir: %w", err)
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
	inputPath := filepath.Join(s.cfg.TempDir, itemID.String()+".mp4")
	outputPath := filepath.Join(s.cfg.TempDir, itemID.String()+"_preview.mp4")
	defer func() {
		_ = os.Remove(inputPath)
		_ = os.Remove(outputPath)
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
	if err := s.markReady(ctx, itemID, previewURL); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}

	s.logger.Info("transcode complete",
		"item_id", itemID,
		"user_id", userID,
		"preview_url", previewURL,
		"dur", time.Since(start),
	)

	// Параллельный outbox-event: после готовности preview переиндексировать
	// feed_videos в OS, чтобы preview_url улетел туда (фид использует его).
	// Не критично если этот UPDATE упадёт — preview всё равно отдаётся
	// напрямую через /me/portfolio и публичную ручку профиля.
	return s.emitReindex(ctx, userID)
}

// claim — атомарно перевести запись в processing. true = взяли, false =
// уже занято/готово/удалено. Допускаем pending → processing и
// failed → processing (повторная попытка вручную или после patch'a).
func (s *Service) claim(ctx context.Context, itemID uuid.UUID) (bool, error) {
	tag, err := s.cfg.DB.Exec(ctx, `
UPDATE portfolio_items
SET preview_status = 'processing',
    preview_error  = NULL,
    updated_at     = now()
WHERE id = $1
  AND preview_status IN ('pending', 'failed')`, itemID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// markReady — финальный UPDATE: ready + preview_url. Защищаемся через
// WHERE preview_status='processing' от перезаписи ручного revert'a
// админа.
func (s *Service) markReady(ctx context.Context, itemID uuid.UUID, previewURL string) error {
	_, err := s.cfg.DB.Exec(ctx, `
UPDATE portfolio_items
SET preview_url           = $2,
    preview_status        = 'ready',
    preview_error         = NULL,
    preview_generated_at  = now(),
    updated_at            = now()
WHERE id = $1 AND preview_status = 'processing'`, itemID, previewURL)
	return err
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
		return fmt.Errorf("%s: %w", stage, err)
	}
	// Транзиентная ошибка: оставляем processing, lease сам откатит запись.
	return fmt.Errorf("%s: %w", stage, err)
}

// emitReindex шлёт specialist.upserted чтобы feed_videos в OS подхватил
// новый preview_url. Best-effort: ошибка не валит pipeline.
func (s *Service) emitReindex(ctx context.Context, userID uuid.UUID) error {
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil // best-effort
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
		outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()}); err != nil {
		return nil
	}
	_ = tx.Commit(ctx)
	return nil
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
