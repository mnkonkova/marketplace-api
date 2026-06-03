// cmd/s3-key-probe — diagnostic: проверяет какие IAM-permissions есть у
// двух пар ключей (основной upload-only и sweep). Не оставляет следов —
// пробует List + RemoveObject на заведомо несуществующий ключ.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"

	"marketpclce/internal/config"
	"marketpclce/internal/platform/s3"
)

func probe(label string, c *s3.Client) {
	fmt.Printf("=== %s ===\n", label)

	listErr := c.ListObjects(context.Background(), "", func(string, time.Time) bool {
		return false // первый объект — выходим
	})
	if listErr != nil {
		fmt.Printf("  list:   FAIL (%v)\n", listErr)
	} else {
		fmt.Println("  list:   OK")
	}

	delErr := c.RemoveObject(context.Background(),
		"this-key-does-not-exist-just-permission-probe-9f4a")
	if delErr != nil {
		fmt.Printf("  delete: FAIL (%v)\n", delErr)
	} else {
		fmt.Println("  delete: OK (S3 идемпотентен на missing key, значит permission есть)")
	}
}

func main() {
	_ = godotenv.Load()
	cfg, _ := config.Load()

	if cfg.S3AccessKey != "" {
		c, err := s3.New(s3.Config{
			Endpoint: cfg.S3Endpoint, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
			Bucket: cfg.S3Bucket, Region: cfg.S3Region, UseSSL: cfg.S3UseSSL, PublicURL: cfg.S3PublicURL,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "upload key init:", err)
		} else {
			probe("upload key (S3_ACCESS_KEY)", c)
		}
	}
	if cfg.S3SweepAccessKey != "" {
		c, err := s3.New(s3.Config{
			Endpoint: cfg.S3Endpoint, AccessKey: cfg.S3SweepAccessKey, SecretKey: cfg.S3SweepSecretKey,
			Bucket: cfg.S3Bucket, Region: cfg.S3Region, UseSSL: cfg.S3UseSSL, PublicURL: cfg.S3PublicURL,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "sweep key init:", err)
		} else {
			probe("sweep key (S3_SWEEP_ACCESS_KEY)", c)
		}
	}
}
