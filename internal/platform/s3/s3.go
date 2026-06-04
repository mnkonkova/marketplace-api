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
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Config struct {
	Endpoint    string // напр. https://storage.yandexcloud.net (с http(s)://)
	AccessKey   string
	SecretKey   string
	Bucket      string
	Region      string // YC: ru-central1; AWS — свой
	UseSSL      bool
	PublicURL   string // опциональный CNAME, иначе берём ${endpoint}/${bucket}
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

// ListObjects — стримит все объекты под prefix. yield вызывается на каждый
// объект; если возвращает false, итерация останавливается. Возвращаемая
// ошибка — фатальная (битый канал / отказ S3); per-object ошибки приходят
// через obj.Err внутри MinIO-канала и тоже превращаются в return.
//
// Используется sweep'ом orphan media: проходим portfolio/ и images/ под
// тысячи объектов, нужен стрим без буферизации (память O(1)).
func (c *Client) ListObjects(ctx context.Context, prefix string, yield func(key string, lastModified time.Time) bool) error {
	for obj := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("list %s: %w", prefix, obj.Err)
		}
		if !yield(obj.Key, obj.LastModified) {
			return nil
		}
	}
	return ctx.Err()
}

// RemoveObject — удаляет один объект. Идемпотентно (повторный вызов на
// отсутствующий ключ — не ошибка для S3 API).
func (c *Client) RemoveObject(ctx context.Context, key string) error {
	if err := c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("remove %s: %w", key, err)
	}
	return nil
}

// CompletePart — пара (partNumber, etag) для завершения multipart. Нужна,
// чтобы хендлеры не зависели от minio-go типов напрямую.
type CompletePart struct {
	PartNumber int
	ETag       string
}

// NewMultipartUpload — инициирует S3 multipart upload, возвращает uploadID.
// Клиент дальше льёт по чанкам через presigned PUT (см. PresignPart) и
// финалит через CompleteMultipart.
func (c *Client) NewMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	core := minio.Core{Client: c.mc}
	uploadID, err := core.NewMultipartUpload(ctx, c.bucket, key, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return "", fmt.Errorf("new multipart: %w", err)
	}
	return uploadID, nil
}

// PresignPart — выдаёт presigned PUT URL для конкретной части multipart.
// partNumber ∈ [1, 10000] (ограничение S3). Каждая часть кроме последней
// должна быть ≥ 5 MiB — это валидируется на стороне S3, не здесь.
func (c *Client) PresignPart(
	ctx context.Context,
	key, uploadID string,
	partNumber int,
	expiry time.Duration,
) (string, error) {
	reqParams := url.Values{}
	reqParams.Set("partNumber", strconv.Itoa(partNumber))
	reqParams.Set("uploadId", uploadID)
	u, err := c.mc.Presign(ctx, "PUT", c.bucket, key, expiry, reqParams)
	if err != nil {
		return "", fmt.Errorf("presign part: %w", err)
	}
	return u.String(), nil
}

// CompleteMultipart — финализирует multipart. parts должны быть упорядочены
// по PartNumber. ETag — заголовок, который S3 вернул на PUT каждой части
// (фронт его читает из response.headers.get('ETag') и присылает обратно).
func (c *Client) CompleteMultipart(
	ctx context.Context,
	key, uploadID string,
	parts []CompletePart,
) error {
	mparts := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		mparts[i] = minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	core := minio.Core{Client: c.mc}
	if _, err := core.CompleteMultipartUpload(ctx, c.bucket, key, uploadID, mparts, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("complete multipart: %w", err)
	}
	return nil
}

// AbortMultipart — отменяет multipart upload. S3 удалит уже загруженные части.
// Идемпотентно (повторный вызов на отсутствующий uploadID не считается фатальной
// ошибкой со стороны клиента).
func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	core := minio.Core{Client: c.mc}
	if err := core.AbortMultipartUpload(ctx, c.bucket, key, uploadID); err != nil {
		return fmt.Errorf("abort multipart: %w", err)
	}
	return nil
}

// KeyFromURL — обратное от PublicURL: восстанавливает S3-ключ из публичного
// URL. Если URL не принадлежит нашему bucket'у (внешние ссылки на YouTube,
// Vimeo и т.п.) — возвращает "". Используется в sweep'е, чтобы собрать
// referenced-set по avatar_url / video_url / thumbnail_url.
func (c *Client) KeyFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	prefix := c.publicURL + "/"
	if !strings.HasPrefix(rawURL, prefix) {
		return ""
	}
	return strings.TrimPrefix(rawURL, prefix)
}

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
