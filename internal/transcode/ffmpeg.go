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
//   -preset veryfast -crf 28 -c:a aac -b:a 64k -ac 1 -movflags +faststart
//
// Битый контейнер / "Invalid data found" в stderr → ErrPermanent.
// Сам non-zero exit ffmpeg возвращает обычной ошибкой → транзиентно
// (lease воркера откатит и попробует снова).
//
// Звук: AAC mono 64kbps — портфолио-видео могут быть UGC/showreel/реклама,
// где звук = часть работы. 64kbps моно для 8 сек = ~50KB, общий размер
// preview'a растёт с ~500KB до ~550KB. Видимости одного видео за раз
// в feed-view достаточно — играет только active article, default muted.
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
		// Звук: AAC mono 64kbps. -ac 1 = моно (хватает для UGC/voice/music
		// identification), -b:a 64k = достаточно для понятной речи без
		// артефактов. Для 8 сек ≈ 50KB, общий preview ~550KB.
		"-c:a", "aac",
		"-b:a", "64k",
		"-ac", "1",
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

// ProbeDuration возвращает длительность видео в секундах через ffprobe
// (стандартный binary рядом с ffmpeg). 0, err → нет ffprobe / битый файл;
// caller'у решать (можно использовать дефолтные gif-params для среднего
// бакета). Минимальная зависимость: ffprobe идёт в большинстве дистрибов
// в одном пакете с ffmpeg, на VDS уже есть.
func (f *FFmpegBin) ProbeDuration(ctx context.Context, input string) (float64, error) {
	ffprobe := strings.TrimSuffix(f.bin, "ffmpeg") + "ffprobe"
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		input,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	s := strings.TrimSpace(string(out))
	var d float64
	if _, err := fmt.Sscanf(s, "%f", &d); err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}

// GifParams — параметры animated WebP «гифки», выбираемые по длительности
// оригинала. См. docs/VIDEO_TRANSCODING.md §11.
type GifParams struct {
	OffsetSec   int // -ss
	DurationSec int // -t (0 = весь файл)
	FPS         int // fps в -vf
	Quality     int // libwebp -quality (0-100)
}

// ChooseGifParams — выбор параметров под длительность оригинала.
//
//	≤ 5 сек     → 0..весь × fps 15 × q 85 (≈60-80 кадров)
//	5-15 сек    → 2..весь-2 × fps 15 × q 82 (≈75-150 кадров)
//	> 15 сек    → 2..10 × fps 12 × q 78 (≈100 кадров)
//
// После tune-апа (2026-06): scale 320 (было 240), fps и quality подняты,
// потолок -fs 350K (было 200K). Размер ~180-280 KB вместо 80-150 KB.
// На главной 4 hero-ролика = ~1 МБ суммарно. См. docs/VIDEO_TRANSCODING.md §11.
func ChooseGifParams(durationSec float64) GifParams {
	switch {
	case durationSec <= 5:
		return GifParams{OffsetSec: 0, DurationSec: 0, FPS: 15, Quality: 85}
	case durationSec <= 15:
		return GifParams{OffsetSec: 2, DurationSec: 0, FPS: 15, Quality: 82}
	default:
		return GifParams{OffsetSec: 2, DurationSec: 10, FPS: 12, Quality: 78}
	}
}

// MakeAnimatedWebP — pipeline для «гифки»:
//
//	-ss <OFFSET> -i <input> -t <DUR> -vf "fps=<FPS>,scale=240:-2"
//	-loop 0 -c:v libwebp -compression_level 6 -quality <Q> -an
//	-fs 200K
//
// Sequential к MakePreview (mp4) — peak RAM не растёт. DurationSec=0
// означает «весь оставшийся файл после OffsetSec».
//
// Caller (service.go) после успешного pass'а делает os.Stat output'a;
// если размер > 150KB, повторно вызывает MakeAnimatedWebP с
// `params.Quality - 10`. Это даёт chance уложиться без увеличения
// сложности обёртки.
func (f *FFmpegBin) MakeAnimatedWebP(ctx context.Context, input, output string, p GifParams) error {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	args := []string{"-y"}
	if p.OffsetSec > 0 {
		args = append(args, "-ss", fmt.Sprintf("%d", p.OffsetSec))
	}
	args = append(args, "-i", input)
	if p.DurationSec > 0 {
		args = append(args, "-t", fmt.Sprintf("%d", p.DurationSec))
	}
	args = append(args,
		"-vf", fmt.Sprintf("fps=%d,scale=320:-2:flags=lanczos", p.FPS),
		"-loop", "0",
		"-c:v", "libwebp",
		"-compression_level", "6",
		"-quality", fmt.Sprintf("%d", p.Quality),
		"-an",
		"-fs", "350K",
		"-threads", "2",
		output,
	)

	cmd := exec.CommandContext(ctx, f.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrText := stderr.String()
		if isPermanentFFmpegError(stderrText) {
			return fmt.Errorf("%w: %s", ErrPermanent, summarizeFFmpegError(stderrText))
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("ffmpeg webp timeout after %s: %s", f.timeout, summarizeFFmpegError(stderrText))
		}
		return fmt.Errorf("ffmpeg webp failed: %w: %s", err, summarizeFFmpegError(stderrText))
	}
	return nil
}

// ExtractThumbnail — один JPG-кадр на позиции offsetSec. Используется
// регенерацией poster'ов (cmd/regen-thumbs): раньше фронт брал кадр
// на 1.5с, часто попадал в чёрный fade-in. Теперь 2с — большинство
// роликов уже открылись. -q:v 3 = высокое качество JPEG (2-4 — визуально
// без артефактов, весит ~50-80 КБ на 720p).
func (f *FFmpegBin) ExtractThumbnail(ctx context.Context, input, output string, offsetSec float64) error {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	args := []string{
		"-y",
		// -ss перед -i = fast seek по keyframes (менее точно, но за секунды
		// не критично, и не декодит первые 2с зря).
		"-ss", fmt.Sprintf("%.2f", offsetSec),
		"-i", input,
		"-frames:v", "1",
		"-vf", "scale=-2:720:flags=lanczos",
		"-q:v", "3",
		"-threads", "2",
		output,
	}
	cmd := exec.CommandContext(ctx, f.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := stderr.String()
		if isPermanentFFmpegError(text) {
			return fmt.Errorf("%w: %s", ErrPermanent, summarizeFFmpegError(text))
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("ffmpeg thumb timeout after %s: %s", f.timeout, summarizeFFmpegError(text))
		}
		return fmt.Errorf("ffmpeg thumb failed: %w: %s", err, summarizeFFmpegError(text))
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
