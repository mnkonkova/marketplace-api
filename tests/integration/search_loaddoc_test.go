package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/profiles"
	"marketpclce/internal/search"
	"marketpclce/tests/integration"
)

// TestLoadDoc_PreviewVideoFieldsDenormalized — проверяет что LoadDoc
// возвращает preview_video_url / preview_thumb_url для спеца с видео.
// Ранее эти поля индексировались отдельным round-trip'ом на фронте
// (N+1 GET /feed?ids=... для каждой карточки в SearchResultsPage);
// после денормализации в IndexDoc фронт получает всё одним запросом.
func TestLoadDoc_PreviewVideoFieldsDenormalized(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := profiles.NewService(profiles.NewRepo(pool)).WithMediaStorage(bucketMedia{})
	ctx := context.Background()

	// Заливаем видео в портфолио — репо использует его как preview.
	videoURL := "https://stub.test/portfolio/" + uid.String() + "/v.mp4"
	thumbURL := "https://stub.test/portfolio/" + uid.String() + "/t.jpg"
	_, err := svc.AddPortfolioVideo(ctx, uid, profiles.PortfolioCreateInput{
		VideoURL:     videoURL,
		ThumbnailURL: thumbURL,
		Title:        "Test video",
	})
	if err != nil {
		t.Fatalf("add video: %v", err)
	}

	repo := search.NewRepo(pool)
	doc, err := repo.LoadDoc(ctx, uid)
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}

	// Preview_video_url = либо preview_url (если готов) либо video_url.
	// Сразу после создания preview ещё не готов, transcoder не отработал,
	// значит fallback на video_url.
	if doc.PreviewVideoURL != videoURL {
		t.Errorf("PreviewVideoURL = %q, want %q (fallback на video_url когда preview не готов)",
			doc.PreviewVideoURL, videoURL)
	}
	if doc.PreviewThumbURL != thumbURL {
		t.Errorf("PreviewThumbURL = %q, want %q", doc.PreviewThumbURL, thumbURL)
	}
}

// TestLoadDoc_NoVideoEmptyPreview — спец без видео → пустые preview-поля.
func TestLoadDoc_NoVideoEmptyPreview(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)

	repo := search.NewRepo(pool)
	doc, err := repo.LoadDoc(context.Background(), uid)
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if doc.PreviewVideoURL != "" {
		t.Errorf("PreviewVideoURL = %q, want empty (у спеца нет видео)", doc.PreviewVideoURL)
	}
	if doc.PreviewThumbURL != "" {
		t.Errorf("PreviewThumbURL = %q, want empty", doc.PreviewThumbURL)
	}
}

// TestLoadDoc_UnknownUser — несуществующий user_id → ErrNotFound.
func TestLoadDoc_UnknownUser(t *testing.T) {
	pool := integration.Pool(t)
	repo := search.NewRepo(pool)
	_, err := repo.LoadDoc(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("want ErrNotFound for unknown user, got nil")
	}
	if err.Error() != search.ErrNotFound.Error() {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
