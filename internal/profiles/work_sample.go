package profiles

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/google/uuid"
)

// Проба работы к заявке «под ключ».
//
// Креатор отвечает на рассылку роликом. Ролик может быть уже в
// портфолио — тогда грузить нечего, — а может быть снят под эту
// заявку, и вот его надо куда-то положить.
//
// Отдельная ручка, а не переиспользованный аплоад портфолио, по двум
// причинам. Первая: у портфолио есть потолок в двадцать видео, и
// человек с полным портфолио не смог бы ответить на заявку вовсе.
// Вторая: ключ. Проба лежит под префиксом orders/, и именно он даёт
// подметальщику право её не трогать — см. SweepOrphanMedia, где список
// префиксов и список ссылок обязаны сходиться.
//
// ⚠️ Файл живёт по ссылке в order_candidate_responses.file_url. Эта
// колонка есть в Repo.LoadReferencedMediaURLs, а "orders/" — в
// prefixes SweepOrphanMedia. Уберёте одно из двух — либо живые пробы
// начнут исчезать через S3_ORPHAN_MIN_AGE, либо мусор осядет в бакете
// навсегда.

// CreateWorkSampleUploadURL — presigned PUT для пробы работы.
//
// Ограничения те же, что у портфолио (mp4/mov, 50 МБ одним PUT): файл
// тот же по смыслу, и разрешать здесь больше значило бы завести второй
// набор правил на одно и то же.
func (s *Service) CreateWorkSampleUploadURL(
	ctx context.Context, userID uuid.UUID, in PortfolioUploadURLInput,
) (PortfolioUploadURL, error) {
	if s.media == nil {
		return PortfolioUploadURL{}, errors.New("media storage not configured")
	}
	ext, ok := allowedUploadTypes[in.ContentType]
	if !ok {
		return PortfolioUploadURL{}, fmt.Errorf(
			"%w: content_type must be video/mp4 or video/quicktime", ErrInvalidInput)
	}
	if in.SizeBytes <= 0 {
		return PortfolioUploadURL{}, fmt.Errorf("%w: size_bytes is required", ErrInvalidInput)
	}
	if in.SizeBytes > portfolioMaxUploadBytes {
		return PortfolioUploadURL{}, fmt.Errorf("%w: file too large (max %d MB)",
			ErrInvalidInput, portfolioMaxUploadBytes/(1024*1024))
	}

	key := path.Join("orders", userID.String(), uuid.NewString()+ext)
	uploadURL, err := s.media.PresignPut(ctx, key, in.ContentType, portfolioUploadExpiry)
	if err != nil {
		return PortfolioUploadURL{}, fmt.Errorf("presign: %w", err)
	}
	return PortfolioUploadURL{
		UploadURL: uploadURL,
		PublicURL: s.media.PublicURL(key),
		Key:       key,
		ExpiresIn: int(portfolioUploadExpiry.Seconds()),
	}, nil
}
