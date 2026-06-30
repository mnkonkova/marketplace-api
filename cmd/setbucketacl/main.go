// Одноразовый CLI для выставления public-read policy на bucket в Yandex
// Object Storage. Запуск: go run ./cmd/setbucketacl. Читает S3_* из .env.
//
// Зачем: dev-bucket создан без анонимного public read → публичные URL фото
// отдают 403. Эта утилита ставит JSON-policy "Allow s3:GetObject for *".
// Один раз на bucket — после этого все объекты доступны анонимно.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"marketpclce/internal/config"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.S3Bucket == "" || cfg.S3AccessKey == "" {
		log.Fatal("S3_BUCKET / S3_ACCESS_KEY пусты в .env")
	}
	// endpoint без https:// — minio сам добавит.
	endpoint := cfg.S3Endpoint
	for _, p := range []string{"https://", "http://"} {
		if len(endpoint) > len(p) && endpoint[:len(p)] == p {
			endpoint = endpoint[len(p):]
		}
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3UseSSL,
		Region: cfg.S3Region,
	})
	if err != nil {
		log.Fatalf("minio client: %v", err)
	}
	// Bucket-policy только для анонимного public-read объектов. Write-доступ
	// нашему API даётся через IAM-роль `storage.editor` на service account
	// (не через CanonicalUser в bucket-policy) — это чище: новые SA получают
	// доступ через IAM без переписи policy, и нет риска захардкодить чужие
	// canonical IDs (как было в первой версии — она перетёрла прод-policy).
	policy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Sid": "PublicReadGetObject",
			"Effect": "Allow",
			"Principal": "*",
			"Action": ["s3:GetObject"],
			"Resource": ["arn:aws:s3:::%s/*"]
		}]
	}`, cfg.S3Bucket)
	if err := client.SetBucketPolicy(context.Background(), cfg.S3Bucket, policy); err != nil {
		log.Fatalf("set policy: %v", err)
	}
	fmt.Printf("OK: public-read policy set on bucket %q\n", cfg.S3Bucket)
}
