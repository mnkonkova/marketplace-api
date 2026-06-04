package profiles

import (
	"context"

	"marketpclce/internal/platform/s3"
)

// NewS3MediaStorage — адаптер s3.Client → MediaStorage. Нужен только из-за
// CompleteMultipart: s3.Client принимает []s3.CompletePart, а интерфейс
// домена использует []MultipartPart, чтобы не тянуть minio-go в сервис.
// Все остальные методы s3.Client сатисфайят интерфейс напрямую (embed).
func NewS3MediaStorage(c *s3.Client) MediaStorage {
	return &s3MediaStorage{Client: c}
}

type s3MediaStorage struct {
	*s3.Client
}

func (s *s3MediaStorage) CompleteMultipart(
	ctx context.Context,
	key, uploadID string,
	parts []MultipartPart,
) error {
	sp := make([]s3.CompletePart, len(parts))
	for i, p := range parts {
		sp[i] = s3.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	return s.Client.CompleteMultipart(ctx, key, uploadID, sp)
}

// Sanity-check: компилятор разломится если интерфейс изменится.
var _ MediaStorage = (*s3MediaStorage)(nil)