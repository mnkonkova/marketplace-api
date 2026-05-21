package profiles

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

var (
	ErrInvalidInput     = errors.New("invalid input")
	ErrProfileRejected  = errors.New("profile rejected")
	ErrPublishIncomplete = errors.New("publish incomplete")
)

type ProfileChecker interface {
	Available() bool
	Check(ctx context.Context, in CheckInput) (CheckResult, error)
}

type CheckInput struct {
	Bio                  string
	DisplayName          string
	PrimaryCategory      string
	PrimaryCategoryTitle string
}

type PartResult struct {
	OK         bool     `json:"ok"`
	Score      int      `json:"score"`
	Reasons    []string `json:"reasons"`
	Suggestion string   `json:"suggestion"`
}

type CheckResult struct {
	OK   bool       `json:"ok"`
	Bio  PartResult `json:"bio"`
	Name PartResult `json:"name"`
}

// MediaStorage — абстракция над S3-совместимым хранилищем. Сервис не знает
// про minio/aws — только про presigned PUT и сборку public URL. main.go
// внедряет реализацию через WithMediaStorage; nil = аплоад выключен.
type MediaStorage interface {
	PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error)
	PublicURL(key string) string
}

type Service struct {
	repo    *Repo
	checker ProfileChecker
	media   MediaStorage
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) WithProfileChecker(c ProfileChecker) *Service {
	s.checker = c
	return s
}

func (s *Service) WithMediaStorage(m MediaStorage) *Service {
	s.media = m
	return s
}

func (s *Service) MediaAvailable() bool { return s.media != nil }

func (s *Service) Get(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return s.repo.Get(ctx, userID)
}

func (s *Service) GetPublic(ctx context.Context, userID uuid.UUID) (PublicProfile, error) {
	return s.repo.GetPublic(ctx, userID)
}

func (s *Service) Patch(ctx context.Context, userID uuid.UUID, in PatchInput) (Profile, error) {
	if in.DisplayName != nil {
		v := strings.TrimSpace(*in.DisplayName)
		if v == "" {
			return Profile{}, fmt.Errorf("%w: display_name cannot be empty", ErrInvalidInput)
		}
		in.DisplayName = &v
	}
	if in.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Currency))
		if len(c) != 3 {
			return Profile{}, fmt.Errorf("%w: currency must be 3-letter code", ErrInvalidInput)
		}
		in.Currency = &c
	}
	if in.RateMin != nil && *in.RateMin < 0 {
		return Profile{}, fmt.Errorf("%w: rate_min must be >= 0", ErrInvalidInput)
	}
	if in.RateMax != nil && *in.RateMax < 0 {
		return Profile{}, fmt.Errorf("%w: rate_max must be >= 0", ErrInvalidInput)
	}
	if in.RateMin != nil && in.RateMax != nil && *in.RateMin > *in.RateMax {
		return Profile{}, fmt.Errorf("%w: rate_min must be <= rate_max", ErrInvalidInput)
	}
	// Контакты — лёгкий тримминг и cap по длине. Формат не валидируем
	// строго (телефоны бывают всякие, e-mail с unicode-доменами и т.п.) —
	// специалист сам понимает что вписывает; оно нигде не парсится автоматически.
	if in.ContactEmail != nil {
		v := strings.TrimSpace(*in.ContactEmail)
		if len(v) > 254 {
			return Profile{}, fmt.Errorf("%w: contact_email too long", ErrInvalidInput)
		}
		in.ContactEmail = &v
	}
	if in.ContactPhone != nil {
		v := strings.TrimSpace(*in.ContactPhone)
		if len(v) > 64 {
			return Profile{}, fmt.Errorf("%w: contact_phone too long", ErrInvalidInput)
		}
		in.ContactPhone = &v
	}

	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.PatchInTx(ctx, tx, userID, in); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

func (s *Service) SetCategories(ctx context.Context, userID uuid.UUID, in SetCategoriesInput) (Profile, error) {
	codes := dedupStrings(in.Codes)
	if len(codes) == 0 {
		return Profile{}, fmt.Errorf("%w: at least one category is required", ErrInvalidInput)
	}
	if in.Primary == "" {
		return Profile{}, fmt.Errorf("%w: primary category is required", ErrInvalidInput)
	}
	if !contains(codes, in.Primary) {
		return Profile{}, fmt.Errorf("%w: primary must be in codes", ErrInvalidInput)
	}

	valid, err := s.repo.ValidCategoryCodes(ctx, codes)
	if err != nil {
		return Profile{}, err
	}
	if len(valid) != len(codes) {
		return Profile{}, fmt.Errorf("%w: unknown category code", ErrInvalidInput)
	}

	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.LockProfileForUpdateInTx(ctx, tx, userID, in.UpdatedAt); err != nil {
			return err
		}
		if err := s.repo.ReplaceCategoriesInTx(ctx, tx, userID, codes, in.Primary); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]any{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

func (s *Service) SetSkills(ctx context.Context, userID uuid.UUID, in SetSkillsInput) (Profile, error) {
	ids := make([]uuid.UUID, 0, len(in.SkillIDs))
	seen := map[uuid.UUID]struct{}{}
	for _, raw := range in.SkillIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return Profile{}, fmt.Errorf("%w: bad skill id %q", ErrInvalidInput, raw)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	valid, err := s.repo.ValidSkillIDs(ctx, ids)
	if err != nil {
		return Profile{}, err
	}
	if len(valid) != len(ids) {
		return Profile{}, fmt.Errorf("%w: unknown skill id", ErrInvalidInput)
	}

	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.LockProfileForUpdateInTx(ctx, tx, userID, in.UpdatedAt); err != nil {
			return err
		}
		if err := s.repo.ReplaceSkillsInTx(ctx, tx, userID, ids); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]any{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

func (s *Service) SetPublished(ctx context.Context, userID uuid.UUID, published bool) (Profile, error) {
	event := outbox.EventSpecialistPublished
	if !published {
		event = outbox.EventSpecialistRetracted
	}
	if published && s.checker != nil && s.checker.Available() {
		p, err := s.repo.Get(ctx, userID)
		if err != nil {
			return Profile{}, err
		}
		bio := strings.TrimSpace(p.Bio)
		name := strings.TrimSpace(p.DisplayName)
		if bio == "" {
			return Profile{}, fmt.Errorf("%w: bio is empty", ErrPublishIncomplete)
		}
		if name == "" {
			return Profile{}, fmt.Errorf("%w: display_name is empty", ErrPublishIncomplete)
		}
		title, _ := s.repo.CategoryTitle(ctx, p.PrimaryCategory)
		res, err := s.checker.Check(ctx, CheckInput{
			Bio:                  bio,
			DisplayName:          name,
			PrimaryCategory:      p.PrimaryCategory,
			PrimaryCategoryTitle: title,
		})
		if err != nil {
			return Profile{}, fmt.Errorf("profile check: %w", err)
		}
		if !res.OK {
			return Profile{}, &ProfileRejectedError{Result: res}
		}
	}
	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.SetPublishedInTx(ctx, tx, userID, published); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			event, map[string]any{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

/* ───── portfolio (video) CRUD ───────────────────────────────────
   Сейчас — только URL-форма: специалист хостит видео сам и вставляет
   ссылку. file-upload через S3 (Yandex Object Storage) — следующий шаг,
   ждёт ключей. Когда будет — добавится отдельный handler, который
   аплоадит в бакет и зовёт ту же CreatePortfolioVideo. */

const (
	portfolioMaxVideosPerUser   = 20
	portfolioMaxTitleLen        = 200
	portfolioMaxDescriptionLen  = 1000

	// 50 МБ — синхронизировано с фронтом. Увеличим, когда будет HLS-транскод.
	portfolioMaxUploadBytes = 50 * 1024 * 1024
	portfolioUploadExpiry   = 15 * time.Minute

	// Картинки (аватар + превью к видео): 5 МБ хватает с запасом, типы —
	// jpeg/png/webp. Срок presigned URL короче — меньше окно для misuse.
	imageMaxUploadBytes = 5 * 1024 * 1024
	imageUploadExpiry   = 5 * time.Minute
)

var allowedUploadTypes = map[string]string{
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
}

var allowedImageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

func (s *Service) ListPortfolio(ctx context.Context, userID uuid.UUID) ([]PortfolioItem, error) {
	return s.repo.ListPortfolio(ctx, userID)
}

func (s *Service) AddPortfolioVideo(ctx context.Context, userID uuid.UUID, in PortfolioCreateInput) (PortfolioItem, error) {
	in.VideoURL = strings.TrimSpace(in.VideoURL)
	in.ThumbnailURL = strings.TrimSpace(in.ThumbnailURL)
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)

	if in.VideoURL == "" {
		return PortfolioItem{}, fmt.Errorf("%w: video_url is required", ErrInvalidInput)
	}
	if !isHTTPURL(in.VideoURL) {
		return PortfolioItem{}, fmt.Errorf("%w: video_url must be http(s)", ErrInvalidInput)
	}
	if in.ThumbnailURL != "" && !isHTTPURL(in.ThumbnailURL) {
		return PortfolioItem{}, fmt.Errorf("%w: thumbnail_url must be http(s)", ErrInvalidInput)
	}
	if in.Title == "" {
		return PortfolioItem{}, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if len(in.Title) > portfolioMaxTitleLen {
		return PortfolioItem{}, fmt.Errorf("%w: title too long", ErrInvalidInput)
	}
	if len(in.Description) > portfolioMaxDescriptionLen {
		return PortfolioItem{}, fmt.Errorf("%w: description too long", ErrInvalidInput)
	}
	in.CategoryCodes = dedupStrings(in.CategoryCodes)
	// Категории видео должны быть подмножеством категорий профиля. Если юзер
	// не выбрал — по дефолту ставим primary (видео должно где-то «жить»).
	profile, err := s.repo.Get(ctx, userID)
	if err != nil {
		return PortfolioItem{}, err
	}
	if len(in.CategoryCodes) == 0 {
		if profile.PrimaryCategory != "" {
			in.CategoryCodes = []string{profile.PrimaryCategory}
		}
	} else {
		profileSet := make(map[string]struct{}, len(profile.Categories))
		for _, c := range profile.Categories {
			profileSet[c] = struct{}{}
		}
		for _, c := range in.CategoryCodes {
			if _, ok := profileSet[c]; !ok {
				return PortfolioItem{}, fmt.Errorf("%w: category %q is not in profile categories", ErrInvalidInput, c)
			}
		}
	}

	// hard-лимит: 20 видео на спеца. ОК для MVP, но превышается явной ошибкой,
	// чтобы не молча дропать.
	existing, err := s.repo.ListPortfolio(ctx, userID)
	if err != nil {
		return PortfolioItem{}, err
	}
	videos := 0
	for _, it := range existing {
		if it.VideoURL != "" {
			videos++
		}
	}
	if videos >= portfolioMaxVideosPerUser {
		return PortfolioItem{}, fmt.Errorf("%w: max %d videos", ErrInvalidInput, portfolioMaxVideosPerUser)
	}

	var item PortfolioItem
	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		item, txErr = s.repo.CreatePortfolioVideoInTx(ctx, tx, userID, in)
		if txErr != nil {
			return txErr
		}
		// Outbox-событие: воркер переиндексирует ES-документ спеца,
		// в т.ч. last_video_at — это критично для /feed ранжирования.
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()})
	})
	if err != nil {
		return PortfolioItem{}, err
	}
	return item, nil
}

func (s *Service) DeletePortfolioItem(ctx context.Context, userID, itemID uuid.UUID) error {
	return s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.DeletePortfolioItemInTx(ctx, tx, userID, itemID); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()})
	})
}

// SetPortfolioCategories — обновляет category_codes у видео-айтема.
// Категории должны быть подмножеством категорий профиля специалиста; пустой
// список разрешён (видео не попадёт ни под один категорийный фильтр).
// expectedUpdatedAt — optimistic-lock версия portfolio_items.updated_at,
// прислана фронтом из последнего GET. nil = без проверки.
func (s *Service) SetPortfolioCategories(ctx context.Context, userID, itemID uuid.UUID, codes []string, expectedUpdatedAt *time.Time) (PortfolioItem, error) {
	codes = dedupStrings(codes)
	profile, err := s.repo.Get(ctx, userID)
	if err != nil {
		return PortfolioItem{}, err
	}
	profileSet := make(map[string]struct{}, len(profile.Categories))
	for _, c := range profile.Categories {
		profileSet[c] = struct{}{}
	}
	for _, c := range codes {
		if _, ok := profileSet[c]; !ok {
			return PortfolioItem{}, fmt.Errorf("%w: category %q is not in profile categories", ErrInvalidInput, c)
		}
	}
	var item PortfolioItem
	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		item, txErr = s.repo.UpdatePortfolioCategoriesInTx(ctx, tx, userID, itemID, codes, expectedUpdatedAt)
		if txErr != nil {
			return txErr
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()})
	})
	if err != nil {
		return PortfolioItem{}, err
	}
	return item, nil
}

// CreatePortfolioUploadURL — выдаёт presigned PUT URL для прямого аплоада в S3.
// Сам файл клиент кладёт в YC через возвращённый upload_url; затем
// шлёт POST /me/portfolio с public_url, чтобы создать запись в БД.
// Это разделение даёт две полезные вещи:
//  - наш сервер не проксирует mp4 (нет нагрузки)
//  - запись в БД создаётся только если аплоад реально прошёл
func (s *Service) CreatePortfolioUploadURL(
	ctx context.Context,
	userID uuid.UUID,
	in PortfolioUploadURLInput,
) (PortfolioUploadURL, error) {
	if s.media == nil {
		return PortfolioUploadURL{}, errors.New("media storage not configured")
	}

	ext, ok := allowedUploadTypes[in.ContentType]
	if !ok {
		return PortfolioUploadURL{}, fmt.Errorf("%w: content_type must be video/mp4 or video/quicktime", ErrInvalidInput)
	}
	if in.SizeBytes <= 0 {
		return PortfolioUploadURL{}, fmt.Errorf("%w: size_bytes is required", ErrInvalidInput)
	}
	if in.SizeBytes > portfolioMaxUploadBytes {
		return PortfolioUploadURL{}, fmt.Errorf("%w: file too large (max %d MB)", ErrInvalidInput, portfolioMaxUploadBytes/(1024*1024))
	}

	// Hard-cap 20 видео — проверяем здесь, чтобы не выдавать URL впустую.
	existing, err := s.repo.ListPortfolio(ctx, userID)
	if err != nil {
		return PortfolioUploadURL{}, err
	}
	videos := 0
	for _, it := range existing {
		if it.VideoURL != "" {
			videos++
		}
	}
	if videos >= portfolioMaxVideosPerUser {
		return PortfolioUploadURL{}, fmt.Errorf("%w: max %d videos", ErrInvalidInput, portfolioMaxVideosPerUser)
	}

	// Ключ: portfolio/{user_id}/{uuid}{.mp4|.mov}. Префикс по user_id даёт
	// удобный ACL/listing для конкретного спеца, а UUID — уникальность даже
	// если юзер переименует исходник.
	key := path.Join("portfolio", userID.String(), uuid.NewString()+ext)

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

// CreateImageUploadURL — presigned PUT для аватара или превью видео.
// Тот же паттерн, что у видео: фронт сам кладёт картинку в S3, потом
// сохраняет public_url туда, куда нужно (avatar_url у профиля или
// thumbnail_url у видео — через PATCH /me/profile или POST /me/portfolio).
func (s *Service) CreateImageUploadURL(
	ctx context.Context,
	userID uuid.UUID,
	in ImageUploadURLInput,
) (PortfolioUploadURL, error) {
	if s.media == nil {
		return PortfolioUploadURL{}, errors.New("media storage not configured")
	}
	ext, ok := allowedImageTypes[in.ContentType]
	if !ok {
		return PortfolioUploadURL{}, fmt.Errorf("%w: content_type must be image/jpeg, image/png or image/webp", ErrInvalidInput)
	}
	if in.SizeBytes <= 0 {
		return PortfolioUploadURL{}, fmt.Errorf("%w: size_bytes is required", ErrInvalidInput)
	}
	if in.SizeBytes > imageMaxUploadBytes {
		return PortfolioUploadURL{}, fmt.Errorf("%w: file too large (max %d MB)", ErrInvalidInput, imageMaxUploadBytes/(1024*1024))
	}

	// Один общий префикс: семантика (аватар vs превью) живёт на клиенте.
	key := path.Join("images", userID.String(), uuid.NewString()+ext)

	uploadURL, err := s.media.PresignPut(ctx, key, in.ContentType, imageUploadExpiry)
	if err != nil {
		return PortfolioUploadURL{}, fmt.Errorf("presign: %w", err)
	}
	return PortfolioUploadURL{
		UploadURL: uploadURL,
		PublicURL: s.media.PublicURL(key),
		Key:       key,
		ExpiresIn: int(imageUploadExpiry.Seconds()),
	}, nil
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

type ProfileRejectedError struct {
	Result CheckResult
}

func (e *ProfileRejectedError) Error() string { return "profile rejected by llm check" }
func (e *ProfileRejectedError) Is(target error) bool {
	return target == ErrProfileRejected
}

func dedupStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
