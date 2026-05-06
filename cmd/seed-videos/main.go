// cmd/seed-videos — однократная заливка вертикальных сэмплов в S3-бакет.
// Идемпотентно: для каждого слота делает HEAD; если объект есть — пропускает.
// Источники — публичные mp4 (Pexels free). Если какой-то URL отвалится,
// просто подмени в seedSet ниже на любой свой mp4 (или положи файлы в
// ./seed-videos/{slug}.mp4 — приоритет у локальных).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"marketpclce/internal/config"
)

// seedVideo — слот в каталоге сэмплов. Slug → имя объекта в бакете
// (`seed/{slug}.mp4`). cmd/seed строит URL из publicBase + этих slug'ов,
// поэтому каталог тут — единственный источник правды.
type seedVideo struct {
	Slug   string
	Source string // публичный URL для скачивания, если локального файла нет
	Title  string
}

// SeedVideoSlugs — список slug'ов в каталоге, в порядке. cmd/seed импортирует
// этот же файл (через build-tag нет, а через main-пакет нельзя), поэтому
// в cmd/seed повторили константу — синхронизированы вручную.
var seedSet = []seedVideo{
	{Slug: "vert-01", Title: "Reels-формат, городская съёмка",
		Source: "https://videos.pexels.com/video-files/4145074/4145074-hd_1080_1920_25fps.mp4"},
	{Slug: "vert-02", Title: "Beauty-съёмка, крупный план",
		Source: "https://videos.pexels.com/video-files/3209828/3209828-hd_1080_1920_25fps.mp4"},
	{Slug: "vert-03", Title: "Food-shorts, ритмичный монтаж",
		Source: "https://videos.pexels.com/video-files/3209376/3209376-hd_1080_1920_30fps.mp4"},
	{Slug: "vert-04", Title: "Городские кадры для блога",
		Source: "https://videos.pexels.com/video-files/4109155/4109155-hd_1080_1920_25fps.mp4"},
	{Slug: "vert-05", Title: "Lifestyle-Reels с движением",
		Source: "https://videos.pexels.com/video-files/5752729/5752729-hd_1080_1920_25fps.mp4"},
}

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		fatal("config", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		fatal("s3 not configured", errors.New("S3_ACCESS_KEY and S3_SECRET_KEY required"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mc, err := newMinio(cfg)
	if err != nil {
		fatal("minio", err)
	}

	publicBase := strings.TrimRight(cfg.S3PublicURL, "/")
	if publicBase == "" {
		// path-style fallback ${endpoint}/${bucket}
		publicBase = strings.TrimRight(cfg.S3Endpoint, "/") + "/" + cfg.S3Bucket
	}

	uploaded, skipped, failed := 0, 0, 0
	for _, v := range seedSet {
		key := "seed/" + v.Slug + ".mp4"
		publicURL := publicBase + "/" + key

		// уже есть?
		if _, err := mc.StatObject(ctx, cfg.S3Bucket, key, minio.StatObjectOptions{}); err == nil {
			slog.Info("skip (exists)", "slug", v.Slug, "url", publicURL)
			skipped++
			continue
		}

		// сначала пробуем локальный файл, иначе Pexels
		body, size, ct, source, err := openSource(ctx, v)
		if err != nil {
			slog.Error("source failed", "slug", v.Slug, "err", err)
			failed++
			continue
		}

		_, err = mc.PutObject(ctx, cfg.S3Bucket, key, body, size, minio.PutObjectOptions{
			ContentType: ct,
		})
		_ = body.Close()
		if err != nil {
			slog.Error("put failed", "slug", v.Slug, "err", err)
			failed++
			continue
		}
		slog.Info("uploaded", "slug", v.Slug, "from", source, "size", size, "url", publicURL)
		uploaded++
	}

	slog.Info("done", "uploaded", uploaded, "skipped", skipped, "failed", failed, "total", len(seedSet))
	if failed > 0 {
		os.Exit(2)
	}
}

// openSource возвращает поток для PutObject. Приоритет:
//  1. локальный файл ./seed-videos/{slug}.mp4 (если есть)
//  2. Source URL из каталога (HTTP GET)
func openSource(ctx context.Context, v seedVideo) (rc io.ReadCloser, size int64, contentType, source string, err error) {
	localPath := filepath.Join("seed-videos", v.Slug+".mp4")
	if st, err := os.Stat(localPath); err == nil && !st.IsDir() {
		f, err := os.Open(localPath)
		if err != nil {
			return nil, 0, "", "", fmt.Errorf("open local: %w", err)
		}
		return f, st.Size(), "video/mp4", localPath, nil
	}

	if v.Source == "" {
		return nil, 0, "", "", errors.New("no local file and no source URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.Source, nil)
	if err != nil {
		return nil, 0, "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "marketpclce-seeder/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, "", "", fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		_ = resp.Body.Close()
		return nil, 0, "", "", fmt.Errorf("download status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "video/mp4"
	}
	// Content-Length может быть -1 для chunked; minio-go это переварит.
	return resp.Body, resp.ContentLength, ct, v.Source, nil
}

func newMinio(cfg config.Config) (*minio.Client, error) {
	host, secure := cfg.S3Endpoint, cfg.S3UseSSL
	if i := strings.Index(cfg.S3Endpoint, "://"); i >= 0 {
		secure = cfg.S3Endpoint[:i] == "https"
		host = cfg.S3Endpoint[i+3:]
	}
	// minio.New не принимает полный URL с путём (например,
	// storage.yandexcloud.net/wayprodmarket/) — обрезаем всё после хоста.
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	return minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: secure,
		Region: cfg.S3Region,
	})
}

func fatal(label string, err error) {
	slog.Error(label, "err", err)
	os.Exit(1)
}
