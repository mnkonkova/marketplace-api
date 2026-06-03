package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SweepOrphanMedia — удаляет из S3 объекты под portfolio/ и images/,
// которые НЕ упомянуты в БД (avatar_url / video_url / thumbnail_url) И
// созданы раньше cutoff = now() - minAge.
//
// Зачем minAge: между моментом «выдали presigned PUT URL» и «сохранили запись
// в БД» проходит время. В это окно объект в S3 уже есть, а ссылки на него
// ещё нет. minAge должен быть заметно больше, чем portfolioUploadExpiry
// (15 минут) — рекомендуется 24h.
//
// Алгоритм:
//  1. Один SELECT — собираем все referenced URLs из БД.
//  2. Через s3.KeyFromURL извлекаем ключи (внешние URL отсекаются).
//  3. Стримим объекты bucket'а под нужными префиксами.
//  4. Для каждого: если ключ в referenced — пропуск; если объект свежее
//     cutoff — пропуск (вдруг in-flight upload); иначе RemoveObject.
//
// Память — O(|referenced|). Per-object ошибки удаления логируются и
// инкрементируют kept (не валим весь sweep из-за одного ключа).
//
// Возвращает (deleted, kept, err). err — фатальная ошибка (БД/листинг).
func (s *Service) SweepOrphanMedia(ctx context.Context, minAge time.Duration) (deleted, kept int, err error) {
	if s.media == nil {
		return 0, 0, errors.New("media storage not configured")
	}
	if minAge <= 0 {
		// Без safety-margin'а можно прибить только что аплоадженный, но ещё не
		// сохранённый в БД объект. Жёстко требуем minAge > 0.
		return 0, 0, errors.New("minAge must be positive")
	}

	urls, err := s.repo.LoadReferencedMediaURLs(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load referenced: %w", err)
	}
	referenced := make(map[string]struct{}, len(urls))
	for _, u := range urls {
		if k := s.media.KeyFromURL(u); k != "" {
			referenced[k] = struct{}{}
		}
	}

	cutoff := time.Now().Add(-minAge)
	// Префиксы синхронизированы с keys в CreatePortfolioUploadURL /
	// CreateImageUploadURL. Если префикс поменяется — добавить сюда тоже.
	prefixes := []string{"portfolio/", "images/"}
	for _, prefix := range prefixes {
		err := s.media.ListObjects(ctx, prefix, func(key string, lastModified time.Time) bool {
			if ctx.Err() != nil {
				return false
			}
			if _, ok := referenced[key]; ok {
				kept++
				return true
			}
			if lastModified.After(cutoff) {
				// Слишком свежий — возможно presigned upload ещё не дошёл
				// до POST /me/portfolio. Подождём следующего sweep'a.
				kept++
				return true
			}
			if rerr := s.media.RemoveObject(ctx, key); rerr != nil {
				slog.Warn("s3 sweep: remove failed", "key", key, "err", rerr)
				kept++
				return true
			}
			deleted++
			return true
		})
		if err != nil {
			return deleted, kept, fmt.Errorf("list %s: %w", prefix, err)
		}
	}
	return deleted, kept, nil
}
