package transcode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// ─── моки для FFmpeg и Storage ─────────────────────────────────────

type mockFFmpeg struct {
	mu             sync.Mutex
	probeDur       float64
	probeErr       error
	webpErr        error
	webpFileSize   int // байтов записать в output после успешного pass'а
	previewErr     error
	calls          []string // лог вызовов для проверки порядка
	webpCallCount  int
	lastWebPParams GifParams
}

func (m *mockFFmpeg) MakePreview(_ context.Context, _, output string) error {
	m.mu.Lock()
	m.calls = append(m.calls, "MakePreview")
	m.mu.Unlock()
	if m.previewErr != nil {
		return m.previewErr
	}
	return os.WriteFile(output, []byte("fake-mp4-content"), 0644)
}

func (m *mockFFmpeg) MakeAnimatedWebP(_ context.Context, _, output string, p GifParams) error {
	m.mu.Lock()
	m.calls = append(m.calls, "MakeAnimatedWebP")
	m.webpCallCount++
	m.lastWebPParams = p
	m.mu.Unlock()
	if m.webpErr != nil {
		return m.webpErr
	}
	if m.webpFileSize > 0 {
		buf := make([]byte, m.webpFileSize)
		return os.WriteFile(output, buf, 0644)
	}
	return os.WriteFile(output, []byte{}, 0644)
}

func (m *mockFFmpeg) ProbeDuration(_ context.Context, _ string) (float64, error) {
	m.mu.Lock()
	m.calls = append(m.calls, "ProbeDuration")
	m.mu.Unlock()
	return m.probeDur, m.probeErr
}

type mockStorage struct {
	mu      sync.Mutex
	uploads map[string]string // key → contentType
	upErr   error
}

func newMockStorage() *mockStorage { return &mockStorage{uploads: map[string]string{}} }

func (m *mockStorage) Download(_ context.Context, _, localPath string) error {
	return os.WriteFile(localPath, []byte("original-bytes"), 0644)
}
func (m *mockStorage) Upload(_ context.Context, key, _, contentType string) error {
	if m.upErr != nil {
		return m.upErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads[key] = contentType
	return nil
}
func (m *mockStorage) PublicURL(key string) string { return "https://s3.example/" + key }

// ─── тесты ──────────────────────────────────────────────────────────

func newSilentService(t *testing.T, ff *mockFFmpeg, st *mockStorage) *Service {
	t.Helper()
	// Минуем NewService (требует DB) — конструируем напрямую для unit-теста
	// чисто на makeAnimatedThumb. Эта функция в DB не лезет.
	return &Service{
		cfg: Config{
			FFmpeg:  ff,
			Storage: st,
			TempDir: t.TempDir(),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestMakeAnimatedThumb_Success_LongVideo(t *testing.T) {
	ff := &mockFFmpeg{probeDur: 30, webpFileSize: 80 * 1024} // 80KB — в норме
	st := newMockStorage()
	svc := newSilentService(t, ff, st)
	itemID := uuid.New()
	in := filepath.Join(svc.cfg.TempDir, "in.mp4")
	out := filepath.Join(svc.cfg.TempDir, "out.webp")
	_ = os.WriteFile(in, []byte("x"), 0644)

	url := svc.makeAnimatedThumb(context.Background(), itemID, in, out, "portfolio/u/i_preview.webp")

	if url == "" {
		t.Fatalf("expected URL, got empty")
	}
	if ff.webpCallCount != 1 {
		t.Errorf("expected 1 webp pass, got %d", ff.webpCallCount)
	}
	if ff.lastWebPParams.DurationSec != 10 || ff.lastWebPParams.FPS != 10 {
		t.Errorf("expected long-bucket params, got %+v", ff.lastWebPParams)
	}
	if got := st.uploads["portfolio/u/i_preview.webp"]; got != "image/webp" {
		t.Errorf("expected image/webp upload, got %q", got)
	}
}

func TestMakeAnimatedThumb_OversizedTriggersSecondPass(t *testing.T) {
	// Первый pass даст 200KB → service делает второй pass с quality-10.
	ff := &mockFFmpeg{probeDur: 10, webpFileSize: 200 * 1024}
	st := newMockStorage()
	svc := newSilentService(t, ff, st)
	in := filepath.Join(svc.cfg.TempDir, "in.mp4")
	out := filepath.Join(svc.cfg.TempDir, "out.webp")
	_ = os.WriteFile(in, []byte("x"), 0644)

	svc.makeAnimatedThumb(context.Background(), uuid.New(), in, out, "k.webp")

	if ff.webpCallCount != 2 {
		t.Errorf("expected 2 webp passes (oversize triggered retry), got %d", ff.webpCallCount)
	}
	// Второй pass — quality 70-10=60.
	if ff.lastWebPParams.Quality != 60 {
		t.Errorf("second-pass quality = %d, want 60", ff.lastWebPParams.Quality)
	}
}

func TestMakeAnimatedThumb_ProbeFailedUsesMiddleBucket(t *testing.T) {
	ff := &mockFFmpeg{probeErr: errors.New("ffprobe not found"), webpFileSize: 80 * 1024}
	st := newMockStorage()
	svc := newSilentService(t, ff, st)
	in := filepath.Join(svc.cfg.TempDir, "in.mp4")
	out := filepath.Join(svc.cfg.TempDir, "out.webp")
	_ = os.WriteFile(in, []byte("x"), 0644)

	url := svc.makeAnimatedThumb(context.Background(), uuid.New(), in, out, "k.webp")

	if url == "" {
		t.Fatalf("expected URL despite probe failure")
	}
	// При probe error мы используем 10 сек = middle bucket
	if ff.lastWebPParams.Quality != 70 || ff.lastWebPParams.FPS != 12 {
		t.Errorf("expected middle-bucket params, got %+v", ff.lastWebPParams)
	}
}

func TestMakeAnimatedThumb_FFmpegFailure_NonFatal(t *testing.T) {
	ff := &mockFFmpeg{probeDur: 8, webpErr: errors.New("libwebp not built")}
	st := newMockStorage()
	svc := newSilentService(t, ff, st)
	in := filepath.Join(svc.cfg.TempDir, "in.mp4")
	out := filepath.Join(svc.cfg.TempDir, "out.webp")
	_ = os.WriteFile(in, []byte("x"), 0644)

	url := svc.makeAnimatedThumb(context.Background(), uuid.New(), in, out, "k.webp")

	if url != "" {
		t.Errorf("expected empty URL on ffmpeg failure (non-critical), got %q", url)
	}
	if len(st.uploads) != 0 {
		t.Errorf("upload should not happen on ffmpeg failure")
	}
}

func TestMakeAnimatedThumb_TinyOutputSkipsUpload(t *testing.T) {
	// ffmpeg вернул success но создал dummy 100-байтовый файл — не грузим.
	ff := &mockFFmpeg{probeDur: 8, webpFileSize: 100}
	st := newMockStorage()
	svc := newSilentService(t, ff, st)
	in := filepath.Join(svc.cfg.TempDir, "in.mp4")
	out := filepath.Join(svc.cfg.TempDir, "out.webp")
	_ = os.WriteFile(in, []byte("x"), 0644)

	url := svc.makeAnimatedThumb(context.Background(), uuid.New(), in, out, "k.webp")

	if url != "" {
		t.Errorf("expected empty URL on tiny output, got %q", url)
	}
	if len(st.uploads) != 0 {
		t.Errorf("upload should be skipped on tiny output")
	}
}

func TestMakeAnimatedThumb_UploadFailure_NonFatal(t *testing.T) {
	ff := &mockFFmpeg{probeDur: 8, webpFileSize: 80 * 1024}
	st := newMockStorage()
	st.upErr = errors.New("s3 503")
	svc := newSilentService(t, ff, st)
	in := filepath.Join(svc.cfg.TempDir, "in.mp4")
	out := filepath.Join(svc.cfg.TempDir, "out.webp")
	_ = os.WriteFile(in, []byte("x"), 0644)

	url := svc.makeAnimatedThumb(context.Background(), uuid.New(), in, out, "k.webp")

	if url != "" {
		t.Errorf("expected empty URL on upload failure")
	}
}
