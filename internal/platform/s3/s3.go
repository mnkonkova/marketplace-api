// Package s3 — тонкая обёртка над minio-go (это просто S3-SDK, не сам
// MinIO) под Yandex Object Storage. Не сама бизнес-логика портфолио
// живёт здесь — только: подключиться, выдать presigned PUT URL, собрать
// публичный GET URL.
package s3

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Config struct {
	Endpoint  string // напр. https://storage.yandexcloud.net (с http(s)://)
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string // YC: ru-central1; AWS — свой
	UseSSL    bool
	PublicURL string // опциональный CNAME, иначе берём ${endpoint}/${bucket}
}

type Client struct {
	mc        *minio.Client
	bucket    string
	publicURL string // без trailing /
}

// New поднимает клиент. Возвращает nil-клиент (но с ошибкой), если ключи пусты —
// чтобы хендлеры могли отработать «storage_disabled» более явно.
func New(cfg Config) (*Client, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: missing credentials")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: missing bucket")
	}

	host, secure, err := parseEndpoint(cfg.Endpoint, cfg.UseSSL)
	if err != nil {
		return nil, err
	}

	mc, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: minio.New: %w", err)
	}

	pub := strings.TrimRight(cfg.PublicURL, "/")
	if pub == "" {
		// path-style fallback: ${scheme}://${host}/${bucket}
		scheme := "http"
		if secure {
			scheme = "https"
		}
		pub = fmt.Sprintf("%s://%s/%s", scheme, host, cfg.Bucket)
	}

	return &Client{mc: mc, bucket: cfg.Bucket, publicURL: pub}, nil
}

// PresignPut — выдаёт URL для прямой загрузки PUT'ом из браузера.
// expiry должен быть в [1m, 7d], YC принимает любые в этом диапазоне.
// contentType прокидываем как обязательный header — клиент должен PUT'нуть
// файл с тем же Content-Type, иначе подпись развалится.
func (c *Client) PresignPut(
	ctx context.Context,
	key, contentType string,
	expiry time.Duration,
) (string, error) {
	headers := url.Values{}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	u, err := c.mc.PresignedPutObject(ctx, c.bucket, key, expiry)
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return u.String(), nil
}

// PublicURL — возвращает URL, по которому объект доступен на чтение
// (для public-read бакета). В private-режиме нужно отдельно генерить
// signed GET — пока не поддерживается.
func (c *Client) PublicURL(key string) string {
	return c.publicURL + "/" + strings.TrimLeft(key, "/")
}

// Bucket — для логирования/диагностики.
func (c *Client) Bucket() string { return c.bucket }

// parseEndpoint вычленяет host и решает про SSL: если в endpoint есть схема,
// она побеждает над UseSSL-флагом, чтобы https://storage.yandexcloud.net
// автоматически шёл по TLS, а http://localhost:9000 — без.
func parseEndpoint(endpoint string, useSSL bool) (host string, secure bool, err error) {
	if endpoint == "" {
		return "", false, errors.New("s3: empty endpoint")
	}
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return "", false, fmt.Errorf("parse endpoint: %w", err)
		}
		return u.Host, u.Scheme == "https", nil
	}
	return endpoint, useSSL, nil
}
