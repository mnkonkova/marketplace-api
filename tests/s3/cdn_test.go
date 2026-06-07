package s3_test

import (
	"strings"
	"testing"

	"marketpclce/internal/platform/s3"
)

// TestCDN_PublicURL_PrefersCDNWhenSet — главное архитектурное свойство:
// PublicURL отдаёт юзерам CDN-домен, чтобы трафик шёл через edge-кеш,
// а origin (Object Storage) видел только miss'ы.
func TestCDN_PublicURL_PrefersCDNWhenSet(t *testing.T) {
	c, err := s3.New(s3.Config{
		Endpoint:   "https://storage.yandexcloud.net",
		AccessKey:  "AK",
		SecretKey:  "SK",
		Bucket:     "marketpclce",
		PublicURL:  "https://media.example.com",
		CDNBaseURL: "https://cdn.example.com",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got := c.PublicURL("portfolio/abc/video.mp4")
	want := "https://cdn.example.com/portfolio/abc/video.mp4"
	if got != want {
		t.Errorf("PublicURL with CDN: got %q want %q", got, want)
	}
	// OriginURL даёт прямой URL в S3 — нужно для s3-sweep и логирования.
	originGot := c.OriginURL("portfolio/abc/video.mp4")
	originWant := "https://media.example.com/portfolio/abc/video.mp4"
	if originGot != originWant {
		t.Errorf("OriginURL: got %q want %q", originGot, originWant)
	}
}

// TestCDN_PublicURL_FallsBackToOrigin — без CDN отдаём origin как было.
// Гарантирует backward compat — деплой без CDN_BASE_URL ничего не ломает.
func TestCDN_PublicURL_FallsBackToOrigin(t *testing.T) {
	c, err := s3.New(s3.Config{
		Endpoint:  "https://storage.yandexcloud.net",
		AccessKey: "AK",
		SecretKey: "SK",
		Bucket:    "marketpclce",
		PublicURL: "https://media.example.com",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got := c.PublicURL("portfolio/abc/video.mp4")
	want := "https://media.example.com/portfolio/abc/video.mp4"
	if got != want {
		t.Errorf("PublicURL without CDN: got %q want %q", got, want)
	}
}

// TestCDN_KeyFromURL_RecognizesBoth — критично для миграции и sweep'a.
// После включения CDN в БД появятся CDN-URL'ы у новых записей, но
// старые останутся с origin'ом. KeyFromURL должен работать с обоими,
// иначе sweep начнёт считать старые URL "сиротами" и пожжёт их.
func TestCDN_KeyFromURL_RecognizesBoth(t *testing.T) {
	c, err := s3.New(s3.Config{
		Endpoint:   "https://storage.yandexcloud.net",
		AccessKey:  "AK",
		SecretKey:  "SK",
		Bucket:     "marketpclce",
		PublicURL:  "https://media.example.com",
		CDNBaseURL: "https://cdn.example.com",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"cdn url", "https://cdn.example.com/portfolio/u/v.mp4", "portfolio/u/v.mp4"},
		{"origin url", "https://media.example.com/portfolio/u/v.mp4", "portfolio/u/v.mp4"},
		{"external url", "https://youtube.com/watch?v=abc", ""},
		{"empty", "", ""},
		{"cdn with trailing slash", "https://cdn.example.com/portfolio/u/v.mp4?x=y", "portfolio/u/v.mp4?x=y"},
	}
	for _, c2 := range cases {
		t.Run(c2.name, func(t *testing.T) {
			got := c.KeyFromURL(c2.url)
			if got != c2.want {
				t.Errorf("KeyFromURL(%q) = %q, want %q", c2.url, got, c2.want)
			}
		})
	}
}

// TestCDN_TrimTrailingSlash — config из env часто прилетает с trailing /,
// проверяем что не дублируется в результате.
func TestCDN_TrimTrailingSlash(t *testing.T) {
	c, err := s3.New(s3.Config{
		Endpoint:   "https://storage.yandexcloud.net",
		AccessKey:  "AK",
		SecretKey:  "SK",
		Bucket:     "marketpclce",
		CDNBaseURL: "https://cdn.example.com/",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got := c.PublicURL("key")
	if strings.Contains(got, "//") && !strings.HasPrefix(got, "https://") {
		t.Errorf("PublicURL должен не иметь двойного / в середине: %q", got)
	}
	if got != "https://cdn.example.com/key" {
		t.Errorf("got %q, want https://cdn.example.com/key", got)
	}
}
