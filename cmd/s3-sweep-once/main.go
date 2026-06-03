// cmd/s3-sweep-once — одноразовый прогон orphan-sweep'a, для верификации и
// ops-кейсов («давайте я вручную сейчас почищу»). Конфиг тот же, что у
// worker'a (S3_*, DATABASE_URL); minAge передаётся через -min-age флаг,
// перекрывая S3_ORPHAN_MIN_AGE из env.
//
// Пример:
//   s3-sweep-once                 # nominal: minAge=24h
//   s3-sweep-once -min-age 10000h # dry-run: никого не удалит, покажет kept
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"marketpclce/internal/config"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/profiles"
)

func main() {
	var minAgeOverride time.Duration
	var dry bool
	flag.DurationVar(&minAgeOverride, "min-age", 0,
		"override S3_ORPHAN_MIN_AGE (например 10000h чтобы не удалять)")
	flag.BoolVar(&dry, "dry", false,
		"печатать orphan-ключи в stdout, ничего не удалять")
	flag.Parse()

	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		fmt.Fprintln(os.Stderr, "s3 creds not set")
		os.Exit(1)
	}
	s3Client, err := s3.New(s3.Config{
		Endpoint:  cfg.S3Endpoint,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
		Bucket:    cfg.S3Bucket,
		Region:    cfg.S3Region,
		UseSSL:    cfg.S3UseSSL,
		PublicURL: cfg.S3PublicURL,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3 init: %v\n", err)
		os.Exit(1)
	}

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	repo := profiles.NewRepo(pool)
	svc := profiles.NewService(repo).WithMediaStorage(s3Client)

	minAge := cfg.S3OrphanMinAge
	if minAgeOverride > 0 {
		minAge = minAgeOverride
	}

	start := time.Now()
	if dry {
		// Дублируем sweep-логику без RemoveObject: печатаем кандидатов в stdout.
		// Сюда же ушёл бы slog при настоящем прогоне. Не зовём Service чтобы
		// не плодить там опцию «dryRun bool» — production-код без флагов.
		urls, err := repo.LoadReferencedMediaURLs(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load referenced:", err)
			os.Exit(1)
		}
		ref := make(map[string]struct{}, len(urls))
		for _, u := range urls {
			if k := s3Client.KeyFromURL(u); k != "" {
				ref[k] = struct{}{}
			}
		}
		cutoff := time.Now().Add(-minAge)
		var orphans, keptRef, keptYoung int
		for _, prefix := range []string{"portfolio/", "images/"} {
			_ = s3Client.ListObjects(ctx, prefix, func(key string, lastModified time.Time) bool {
				if _, ok := ref[key]; ok {
					keptRef++
					fmt.Printf("  KEEP-ref:   %s\n", key)
					return true
				}
				if lastModified.After(cutoff) {
					keptYoung++
					fmt.Printf("  KEEP-young: %s (last_modified=%s)\n", key, lastModified.Format(time.RFC3339))
					return true
				}
				orphans++
				fmt.Printf("  orphan:     %s (last_modified=%s)\n", key, lastModified.Format(time.RFC3339))
				return true
			})
		}
		fmt.Printf("dry-run: orphans=%d kept_ref=%d kept_young=%d referenced_in_db=%d minAge=%s elapsed=%s bucket=%s\n",
			orphans, keptRef, keptYoung, len(ref), minAge, time.Since(start).Round(time.Millisecond), s3Client.Bucket())
		return
	}
	deleted, kept, err := svc.SweepOrphanMedia(ctx, minAge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sweep failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("sweep complete: deleted=%d kept=%d minAge=%s elapsed=%s bucket=%s\n",
		deleted, kept, minAge, time.Since(start).Round(time.Millisecond), s3Client.Bucket())
}
