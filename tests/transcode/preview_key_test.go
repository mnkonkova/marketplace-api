package transcode_test

import (
	"context"
	"testing"

	"marketpclce/internal/transcode"
)

// Smoke-тест: проверяем валидацию NewService на отсутствующие
// зависимости. Полноценные integration-тесты pipeline'a добавим
// когда поставим ffmpeg в worker-образ (см. docs/VIDEO_TRANSCODING.md §10).
func TestNewService_RequiresAllDeps(t *testing.T) {
	cases := []struct {
		name      string
		cfg       transcode.Config
		expectErr bool
	}{
		{"no ffmpeg", transcode.Config{}, true},
		{"no storage", transcode.Config{FFmpeg: stubFFmpeg{}}, true},
		{"no db", transcode.Config{FFmpeg: stubFFmpeg{}, Storage: stubStorage{}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := transcode.NewService(c.cfg, nil)
			if (err != nil) != c.expectErr {
				t.Errorf("expected err=%v, got %v", c.expectErr, err)
			}
		})
	}
}

type stubFFmpeg struct{}

func (stubFFmpeg) MakePreview(_ context.Context, _, _ string) error { return nil }

type stubStorage struct{}

func (stubStorage) Download(_ context.Context, _, _ string) error          { return nil }
func (stubStorage) Upload(_ context.Context, _, _, _ string) error         { return nil }
func (stubStorage) PublicURL(key string) string                            { return "https://s3/" + key }
