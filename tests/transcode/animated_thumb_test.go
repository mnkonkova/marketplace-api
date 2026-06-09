package transcode_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"marketpclce/internal/transcode"
)

// Отдельный FFmpeg-mock для webp-pipeline тестов: позволяет настроить
// размер webp-вывода (для проверки sanity-check'a <1KB) и ошибки
// probe/transcode независимо от mp4-pass'a (который должен пройти всегда).
type webpFFmpeg struct {
	probeDur     float64
	probeErr     error
	webpErr      error
	webpFileSize int  // байтов записать в output после успешного pass'a
	webpCalls    int  // счётчик вызовов MakeAnimatedWebP (для second-pass-теста)
	lastQuality  int  // quality последнего pass'a
}

func (f *webpFFmpeg) MakePreview(_ context.Context, _, output string) error {
	// mp4-pass должен пройти — это unit-тесты webp пайплайна,
	// его поведение проверено в service_test.go::TestProcess_HappyPath.
	return os.WriteFile(output, []byte("fake-mp4"), 0o644)
}

func (f *webpFFmpeg) MakeAnimatedWebP(_ context.Context, _, output string, p transcode.GifParams) error {
	f.webpCalls++
	f.lastQuality = p.Quality
	if f.webpErr != nil {
		return f.webpErr
	}
	buf := make([]byte, f.webpFileSize)
	return os.WriteFile(output, buf, 0o644)
}

func (f *webpFFmpeg) ProbeDuration(_ context.Context, _ string) (float64, error) {
	return f.probeDur, f.probeErr
}

// TestProcess_AnimatedThumb_Success — happy path: pipeline пишет в S3
// _preview.mp4 + _preview.webp, в БД заполнены оба URL.
func TestProcess_AnimatedThumb_Success(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{
		downloads:      map[string][]byte{key: []byte("ORIGINAL")},
		publicURLProto: "https://cdn/",
	}
	ff := &webpFFmpeg{probeDur: 30, webpFileSize: 80 * 1024} // 80KB — в норме
	svc := mkService(t, pool, ff, st)

	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("process: %v", err)
	}

	// DB: оба URL заполнены
	var previewURL, animatedURL string
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(preview_url,''), COALESCE(animated_thumb_url,'')
		   FROM portfolio_items WHERE id=$1`, itemID).
		Scan(&previewURL, &animatedURL)
	if previewURL == "" {
		t.Errorf("expected preview_url filled")
	}
	if animatedURL == "" {
		t.Errorf("expected animated_thumb_url filled")
	}
	// S3: оба ключа залиты
	mp4Key := "portfolio/u/" + itemID.String() + "_preview.mp4"
	webpKey := "portfolio/u/" + itemID.String() + "_preview.webp"
	if _, ok := st.uploaded[mp4Key]; !ok {
		t.Errorf("expected mp4 upload to %s", mp4Key)
	}
	if _, ok := st.uploaded[webpKey]; !ok {
		t.Errorf("expected webp upload to %s", webpKey)
	}
	// Long-bucket parameters: probeDur=30 → quality 65
	if ff.lastQuality != 65 {
		t.Errorf("expected long-bucket quality=65, got %d", ff.lastQuality)
	}
	// Cleanup
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

// TestProcess_AnimatedThumb_Oversized — webp первый pass даёт >150KB,
// service.go делает second pass с quality-10.
func TestProcess_AnimatedThumb_Oversized(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("ORIGINAL")}}
	// probeDur=10 → middle-bucket quality=70. Файл 200K > 150K порог.
	ff := &webpFFmpeg{probeDur: 10, webpFileSize: 200 * 1024}
	svc := mkService(t, pool, ff, st)

	_ = svc.Process(context.Background(), mkPayload(t, itemID, userID, key))

	if ff.webpCalls != 2 {
		t.Errorf("expected 2 webp passes (oversize retry), got %d", ff.webpCalls)
	}
	// Second pass: 70 - 10 = 60
	if ff.lastQuality != 60 {
		t.Errorf("second-pass quality = %d, want 60", ff.lastQuality)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

// TestProcess_AnimatedThumb_ProbeFailedUsesMiddleBucket — ffprobe не отдаёт
// длительность → service фолбэк на middle bucket (10 сек = quality 70).
func TestProcess_AnimatedThumb_ProbeFailedUsesMiddleBucket(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("ORIGINAL")}}
	ff := &webpFFmpeg{probeErr: errors.New("ffprobe not found"), webpFileSize: 80 * 1024}
	svc := mkService(t, pool, ff, st)

	_ = svc.Process(context.Background(), mkPayload(t, itemID, userID, key))

	if ff.lastQuality != 70 {
		t.Errorf("expected middle-bucket quality=70 on probe failure, got %d", ff.lastQuality)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

// TestProcess_AnimatedThumb_FailureNonFatal — webp-pass упал →
// preview_url всё равно записан, animated_thumb_url пуст. Это не валит
// весь pipeline (webp — best-effort, не критично).
func TestProcess_AnimatedThumb_FailureNonFatal(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("ORIGINAL")}}
	ff := &webpFFmpeg{probeDur: 8, webpErr: errors.New("libwebp not built")}
	svc := mkService(t, pool, ff, st)

	if err := svc.Process(context.Background(), mkPayload(t, itemID, userID, key)); err != nil {
		t.Fatalf("process should succeed despite webp failure: %v", err)
	}

	var previewURL, animatedURL string
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(preview_url,''), COALESCE(animated_thumb_url,'')
		   FROM portfolio_items WHERE id=$1`, itemID).
		Scan(&previewURL, &animatedURL)
	if previewURL == "" {
		t.Errorf("expected preview_url (mp4 success despite webp fail)")
	}
	if animatedURL != "" {
		t.Errorf("expected animated_thumb_url empty on webp failure, got %q", animatedURL)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}

// TestProcess_AnimatedThumb_TinyOutputSkipsUpload — ffmpeg вернул success,
// но создал dummy <1KB файл (пограничный кейс). Не загружаем такой в S3.
func TestProcess_AnimatedThumb_TinyOutputSkipsUpload(t *testing.T) {
	pool, cleanup := withPool(t)
	defer cleanup()
	itemID, userID := makeItem(t, pool, "pending", "https://s3/portfolio/u/item.mp4")
	key := "portfolio/u/" + itemID.String() + ".mp4"
	st := &fakeStorage{downloads: map[string][]byte{key: []byte("ORIGINAL")}}
	ff := &webpFFmpeg{probeDur: 8, webpFileSize: 200} // dummy
	svc := mkService(t, pool, ff, st)

	_ = svc.Process(context.Background(), mkPayload(t, itemID, userID, key))

	webpKey := "portfolio/u/" + itemID.String() + "_preview.webp"
	if _, ok := st.uploaded[webpKey]; ok {
		t.Errorf("expected webp upload skipped on tiny output, got it uploaded")
	}
	var animatedURL string
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(animated_thumb_url,'') FROM portfolio_items WHERE id=$1`,
		itemID).Scan(&animatedURL)
	if animatedURL != "" {
		t.Errorf("expected animated_thumb_url empty, got %q", animatedURL)
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE aggregate='specialist' AND aggregate_id=$1`, userID.String())
}
