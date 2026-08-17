package transcode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
	"marketpclce/internal/transcode"
	"marketpclce/tests/integration"
)

// --- mocks ---

type fakeFFmpeg struct {
	err     error
	delay   time.Duration
	written []byte
}

func (f *fakeFFmpeg) MakePreview(ctx context.Context, _, output string) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.err != nil {
		return f.err
	}
	// Имитация: пишем фейковый preview-файл.
	return os.WriteFile(output, f.written, 0o644)
}

// MakeAnimatedWebP / Probe — no-op stubs для удовлетворения
// FFmpeg-интерфейса. Существующие тесты не проверяют webp-pipeline,
// он покрыт отдельно в animated_thumb_test.go.
func (f *fakeFFmpeg) MakeAnimatedWebP(_ context.Context, _, output string, _ transcode.GifParams) error {
	return os.WriteFile(output, []byte{}, 0o644)
}
func (f *fakeFFmpeg) Probe(_ context.Context, _ string) (transcode.VideoMeta, error) {
	return transcode.VideoMeta{Width: 1080, Height: 1920, DurationSec: 10}, nil
}

type fakeStorage struct {
	downloads      map[string][]byte
	downloadErr    error
	uploadErr      error
	uploaded       map[string][]byte
	publicURLProto string
}

func (s *fakeStorage) Download(_ context.Context, key, localPath string) error {
	if s.downloadErr != nil {
		return s.downloadErr
	}
	data, ok := s.downloads[key]
	if !ok {
		return errors.New("not found")
	}
	return os.WriteFile(localPath, data, 0o644)
}

func (s *fakeStorage) Upload(_ context.Context, key, localPath, _ string) error {
	if s.uploadErr != nil {
		return s.uploadErr
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	if s.uploaded == nil {
		s.uploaded = map[string][]byte{}
	}
	s.uploaded[key] = data
	return nil
}

func (s *fakeStorage) PublicURL(key string) string {
	if s.publicURLProto == "" {
		s.publicURLProto = "https://fake.example/"
	}
	return s.publicURLProto + key
}

// --- helpers ---

// withPool — общий setup поверх integration.Pool. Скипает если
// TEST_DATABASE_URL не задан.
func withPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	pool := integration.Pool(t)
	cleanup := func() {
		// no-op: integration.Pool сам управляет жизненным циклом
	}
	return pool, cleanup
}

// makeUser/Item — фабрика для тестов. Создаёт минимально-валидную
// запись portfolio_items + связанного user'a. Возвращает item_id.
func makeItem(t *testing.T, pool *pgxpool.Pool, status, videoURL string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	email := "transcode-" + uuid.NewString() + "@example.com"
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, bio)
VALUES ($1, 'Test Spec', '')`, userID); err != nil {
		t.Fatalf("create spec profile: %v", err)
	}
	var itemID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO portfolio_items (user_id, title, video_url, kind, preview_status, category_codes)
VALUES ($1, 'test', $2, 'video', $3, ARRAY[]::text[]) RETURNING id`,
		userID, videoURL, status).Scan(&itemID); err != nil {
		t.Fatalf("create item: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM portfolio_items WHERE id = $1`, itemID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM specialist_profiles WHERE user_id = $1`, userID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return itemID, userID
}

func mkPayload(t *testing.T, itemID, userID uuid.UUID, key string) []byte {
	t.Helper()
	b, err := json.Marshal(outbox.PortfolioVideoUploadedPayload{
		ItemID: itemID.String(),
		UserID: userID.String(),
		S3Key:  key,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mkService(t *testing.T, pool *pgxpool.Pool, ff transcode.FFmpeg, st *fakeStorage) *transcode.Service {
	t.Helper()
	tempDir := t.TempDir()
	svc, err := transcode.NewService(transcode.Config{
		FFmpeg:             ff,
		Storage:            st,
		TempDir:            tempDir,
		DB:                 pool,
		StuckRecoveryAfter: 100 * time.Millisecond, // быстро для тестов
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// --- tests ---

func TestProcess_HappyPath(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("ORIGINAL")}}
	ff := &fakeFFmpeg{written: []byte("PREVIEW")}
	svc := mkService(t, pool, ff, st)

	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("process: %v", err)
	}

	// 1) запись stored как ready, preview_url не пустой
	var status, previewURL string
	_ = pool.QueryRow(context.Background(),
		`SELECT preview_status, COALESCE(preview_url, '') FROM portfolio_items WHERE id = $1`,
		itemID).Scan(&status, &previewURL)
	if status != "ready" {
		t.Errorf("expected ready, got %q", status)
	}
	if previewURL == "" {
		t.Errorf("expected non-empty preview_url")
	}
	// 2) preview залит в S3 правильным ключом
	expectedKey := "portfolio/u/" + itemID.String() + "_preview.mp4"
	if _, ok := st.uploaded[expectedKey]; !ok {
		t.Errorf("expected upload to %s, got %v", expectedKey, st.uploaded)
	}
	// 3) emitted reindex outbox event для userID
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1 AND event_type='specialist.upserted'`,
		userID.String()).Scan(&n)
	if n == 0 {
		t.Errorf("expected reindex outbox event emitted")
	}
	// Cleanup outbox: не оставляем хвостов для следующих тестов / прод-cleanup.
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

func TestProcess_PermanentErrorMarksFailed(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("BAD")}}
	ff := &fakeFFmpeg{err: fmt.Errorf("%w: invalid data found", transcode.ErrPermanent)}
	svc := mkService(t, pool, ff, st)

	err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key))
	if !errors.Is(err, transcode.ErrPermanent) {
		t.Errorf("expected ErrPermanent, got %v", err)
	}
	var status, errMsg string
	_ = pool.QueryRow(context.Background(),
		`SELECT preview_status, COALESCE(preview_error, '') FROM portfolio_items WHERE id = $1`,
		itemID).Scan(&status, &errMsg)
	if status != "failed" {
		t.Errorf("expected failed, got %q", status)
	}
	if errMsg == "" {
		t.Errorf("expected preview_error set")
	}
}

func TestProcess_ClaimRespectsReadyStatus(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "ready", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	ff := &fakeFFmpeg{written: []byte("ignored")}
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("data")}}
	svc := mkService(t, pool, ff, st)

	// Уже ready → handler должен skip (nil error), запись не меняется.
	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("expected nil (skip), got %v", err)
	}
	if len(st.uploaded) > 0 {
		t.Errorf("ffmpeg/upload не должен запускаться для ready-записи")
	}
}

func TestProcess_StuckProcessingAutoRecovery(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "processing", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"

	// Имитируем «крэш предыдущего хендлера» — двигаем updated_at назад
	// сильнее чем StuckRecoveryAfter (100ms у svc).
	if _, err := pool.Exec(context.Background(),
		`UPDATE portfolio_items SET updated_at = now() - INTERVAL '1 hour' WHERE id = $1`, itemID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	st := &fakeStorage{downloads: map[string][]byte{key: []byte("ORIG")}}
	ff := &fakeFFmpeg{written: []byte("PREVIEW")}
	svc := mkService(t, pool, ff, st)

	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("process: %v", err)
	}
	var status string
	_ = pool.QueryRow(context.Background(),
		`SELECT preview_status FROM portfolio_items WHERE id = $1`, itemID).Scan(&status)
	if status != "ready" {
		t.Errorf("expected auto-recovered to ready, got %q", status)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

func TestProcess_StuckProcessingRecentSkipsRecovery(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "processing", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"

	// updated_at свежий (только что вставлен) → claim должен skip,
	// другой воркер уже держит запись.
	ff := &fakeFFmpeg{written: []byte("would-overwrite")}
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("orig")}}
	svc := mkService(t, pool, ff, st)

	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("expected nil (skip), got %v", err)
	}
	if len(st.uploaded) > 0 {
		t.Errorf("recently-processing запись не должна re-process'иться")
	}
}

func TestProcess_PermanentOnBadPayload(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	svc := mkService(t, pool, &fakeFFmpeg{}, &fakeStorage{})

	// Битый JSON.
	err := svc.Process(context.Background(), []byte(`{not json`))
	if !errors.Is(err, transcode.ErrPermanent) {
		t.Errorf("expected ErrPermanent on bad JSON, got %v", err)
	}

	// Пустой s3_key.
	b, _ := json.Marshal(outbox.PortfolioVideoUploadedPayload{
		ItemID: uuid.NewString(),
		UserID: uuid.NewString(),
		S3Key:  "",
	})
	err = svc.Process(context.Background(), b)
	if !errors.Is(err, transcode.ErrPermanent) {
		t.Errorf("expected ErrPermanent on empty s3_key, got %v", err)
	}
}

func TestProcess_TempDirCleanupOnPipelineEnd(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"

	tempDir := t.TempDir()
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("orig")}}
	ff := &fakeFFmpeg{written: []byte("preview")}
	svc, err := transcode.NewService(transcode.Config{
		FFmpeg:             ff,
		Storage:            st,
		TempDir:            tempDir,
		DB:                 pool,
		StuckRecoveryAfter: 100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("process: %v", err)
	}
	// После пайплайна defer должен удалить и input и output.
	entries, _ := os.ReadDir(tempDir)
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected empty tempdir, leftover: %v", names)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

func TestNewService_StartupCleansOrphanTempFiles(t *testing.T) {
	tempDir := t.TempDir()
	// Имитируем сирот: файлы остались от прошлого инстанса.
	if err := os.WriteFile(filepath.Join(tempDir, "orphan1.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "orphan2_preview.mp4"), []byte("y"), 0o644); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	pool := integration.Pool(t) // skips if no TEST_DATABASE_URL
	svc, err := transcode.NewService(transcode.Config{
		FFmpeg:  &fakeFFmpeg{},
		Storage: &fakeStorage{},
		TempDir: tempDir,
		DB:      pool,
	}, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_ = svc
	entries, _ := os.ReadDir(tempDir)
	if len(entries) != 0 {
		t.Errorf("expected orphans cleaned, got %d", len(entries))
	}
}
