package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// FFmpegBin — реализация FFmpeg через exec'ом локального бинаря.
// Параметры пайплайна зафиксированы — см. docs/VIDEO_TRANSCODING.md §3.
//
// Если ffmpeg не установлен или путь невалиден — NewFFmpegBin вернёт
// ошибку при инициализации, чтобы воркер не стартовал в кривой конфиге.
type FFmpegBin struct {
	bin     string
	timeout time.Duration
}

// NewFFmpegBin резолвит путь до ffmpeg через exec.LookPath (если binPath
// пустой) или принимает явный путь. timeout = верхний потолок на один
// transcode (по умолчанию 90 сек — 480p × 8 сек с veryfast на одном
// vCPU укладывается в ~10-20 сек, 90 даёт запас на холодные старты).
func NewFFmpegBin(binPath string, timeout time.Duration) (*FFmpegBin, error) {
	if binPath == "" {
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("ffmpeg not found in PATH: %w", err)
		}
		binPath = p
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &FFmpegBin{bin: binPath, timeout: timeout}, nil
}

// MakePreview гонит pipeline:
//   -ss 2 -t 8 -vf scale=-2:480 -c:v libx264 -profile:v baseline -level 3.0
//   -preset veryfast -crf 28 -an -movflags +faststart
//
// Битый контейнер / "Invalid data found" в stderr → ErrPermanent.
// Сам non-zero exit ffmpeg возвращает обычной ошибкой → транзиентно
// (lease воркера откатит и попробует снова).
func (f *FFmpegBin) MakePreview(ctx context.Context, input, output string) error {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	args := []string{
		"-y",
		"-ss", "2",                          // skip первые 2 сек
		"-i", input,
		"-t", "8",                           // длительность 8 сек
		"-vf", "scale=-2:480,setsar=1",      // 480p, even-width
		"-c:v", "libx264",
		"-profile:v", "baseline",
		"-level", "3.0",
		"-preset", "veryfast",
		"-crf", "28",
		"-an",                               // без звука
		"-movflags", "+faststart",
		"-fs", "1500K",                      // hard cap (если CRF дал больше)
		// -threads 2: libx264 по дефолту цепляет все ядра CPU (на 6-vCPU
		// VDS — это 12 потоков с lookahead'ом), spawn'ает 12 thread-pool'ов
		// + аллоцирует context на каждый → пик памяти при инициализации
		// сжирает cgroup-лимит контейнера → SIGKILL ещё до первого кадра.
		// 2 потока дают 95% throughput без OOM-разгона.
		"-threads", "2",
		output,
	}

	cmd := exec.CommandContext(ctx, f.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrText := stderr.String()
		if isPermanentFFmpegError(stderrText) {
			return fmt.Errorf("%w: %s", ErrPermanent, summarizeFFmpegError(stderrText))
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("ffmpeg timeout after %s: %s", f.timeout, summarizeFFmpegError(stderrText))
		}
		return fmt.Errorf("ffmpeg failed: %w: %s", err, summarizeFFmpegError(stderrText))
	}
	return nil
}

// isPermanentFFmpegError — эвристика по stderr. Покрывает основные
// «битый файл / неподдерживаемый формат» сигналы. Не покрывает таймауты
// и временные I/O ошибки — те ретраимся.
func isPermanentFFmpegError(stderr string) bool {
	patterns := []string{
		"Invalid data found",
		"moov atom not found",
		"Decoder (codec none) not found",
		"could not find codec parameters",
		"unsupported codec",
		"Invalid argument",
		"Invalid frame size",
	}
	low := strings.ToLower(stderr)
	for _, p := range patterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// summarizeFFmpegError выдёргивает последние 3 строки stderr (там
// обычно реальная причина). Полный stderr на 5-50 КБ — забил бы логи.
func summarizeFFmpegError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	start := len(lines) - 3
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:], " | ")
}
