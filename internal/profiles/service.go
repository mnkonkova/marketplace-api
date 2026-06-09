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
	ErrInvalidInput      = errors.New("invalid input")
	ErrProfileRejected   = errors.New("profile rejected")
	ErrPublishIncomplete = errors.New("publish incomplete")
	// ErrEmailUnverified — soft-gate на publish: пока почта не подтверждена,
	// профиль нельзя опубликовать. Хендлер мапит в 403 email_unverified.
	ErrEmailUnverified = errors.New("email is not verified")
	// ErrUserInactive — юзер деактивирован. Middleware JWT не реchecker'ит,
	// поэтому ловим здесь, до публикации в поиск.
	ErrUserInactive = errors.New("user is inactive")
)

// EmailVerifier — узкий интерфейс soft-gate'а. Реализуется auth.Service.
// Возвращает (active, verified, err). nil-safe: без подключения soft-gate не
// действует (для тестов).
type EmailVerifier interface {
	CheckActiveVerified(ctx context.Context, userID uuid.UUID) (active, verified bool, err error)
}

// ProductionLookup — узкий интерфейс для валидации production_id при
// PatchFull: при выборе продакшена убедиться, что он существует и активен.
// Реализуется productions.Service.IsActiveProduction. nil-safe: без
// подключения сервис не валидирует — FK в БД всё равно отловит несуществующий
// id, но не отличит от деактивированного (захардкоженной строки нет).
type ProductionLookup interface {
	IsActiveProduction(ctx context.Context, id uuid.UUID) (bool, error)
}

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

// MultipartPart — пара (part_number, etag) для финализации multipart.
// Локальный тип, чтобы домен не зависел от minio-go.
type MultipartPart struct {
	PartNumber int
	ETag       string
}

// MediaStorage — абстракция над S3-совместимым хранилищем. Сервис не знает
// про minio/aws — только про presigned PUT, сборку public URL и операции
// для orphan-sweep'a (List/Remove/KeyFromURL). main.go внедряет реализацию
// через WithMediaStorage; nil = аплоад и sweep выключены.
type MediaStorage interface {
	PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error)
	PublicURL(key string) string
	// ListObjects стримит объекты под prefix; yield=false останавливает.
	ListObjects(ctx context.Context, prefix string, yield func(key string, lastModified time.Time) bool) error
	// RemoveObject — удаление одного ключа; идемпотентно.
	RemoveObject(ctx context.Context, key string) error
	// KeyFromURL — обратное от PublicURL; "" если URL не из bucket'а.
	KeyFromURL(rawURL string) string
	// Multipart upload — для крупных файлов (>5 МБ). Фронт льёт чанки сам
	// через presigned PUT, сервер только инициирует/финализирует/отменяет.
	NewMultipartUpload(ctx context.Context, key, contentType string) (uploadID string, err error)
	PresignPart(ctx context.Context, key, uploadID string, partNumber int, expiry time.Duration) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

type Service struct {
	repo       *Repo
	checker    ProfileChecker
	media      MediaStorage
	verifier   EmailVerifier
	production ProductionLookup
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

// WithEmailVerifier подключает soft-gate на publish (требует подтверждённой
// почты). nil-safe: без вызова publish проходит без проверки.
func (s *Service) WithEmailVerifier(v EmailVerifier) *Service {
	s.verifier = v
	return s
}

// WithProductionLookup подключает валидацию production_id при PatchFull.
// Без неё неактивный/несуществующий id даст 23503 (FK) — но активность
// без lookup'а не проверим.
func (s *Service) WithProductionLookup(p ProductionLookup) *Service {
	s.production = p
	return s
}

func (s *Service) MediaAvailable() bool { return s.media != nil }

func (s *Service) Get(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return s.repo.Get(ctx, userID)
}

func (s *Service) GetPublic(ctx context.Context, userID uuid.UUID) (PublicProfile, error) {
	return s.repo.GetPublic(ctx, userID)
}

// PatchFull — атомарный апдейт профиля + (опционально) категорий + (опционально)
// навыков под одной optimistic-lock версией UpdatedAt в ОДНОЙ транзакции.
//
// Проектная мотивация: фронт раньше последовательно дёргал три ручки
// (PATCH /me/profile → PUT /me/profile/categories → PUT /me/profile/skills),
// между ними updated_at в БД успевал бампнуться → второй и третий получали
// 409. Здесь — одна проверка версии, и либо все три части применились,
// либо ни одной.
//
// Семантика nil: каждое поле/секция, оставленные nil во входе, не трогаются.
func (s *Service) PatchFull(ctx context.Context, userID uuid.UUID, in PatchFullInput) (Profile, error) {
	patch := in.toPatchInput()

	// 0. XOR-нормализация production_id / is_freelance.
	if in.ProductionID != nil || in.IsFreelance != nil {
		got, err := NormalizeProduction(ctx, in.ProductionID, in.IsFreelance, s.production)
		if err != nil {
			return Profile{}, err
		}
		patch.SetProduction = got.SetProduction
		patch.ProductionID = got.ProductionID
		patch.SetIsFreelance = got.SetIsFreelance
		patch.IsFreelance = got.IsFreelance
	}

	// 1. Валидация profile-полей (если есть). Дублирует логику из Patch,
	// но вынести её в общий хелпер сейчас — больше шума чем пользы:
	// 9 строк здесь vs новая функция + сигнатура.
	if patch.DisplayName != nil {
		v := strings.TrimSpace(*patch.DisplayName)
		if v == "" {
			return Profile{}, fmt.Errorf("%w: display_name cannot be empty", ErrInvalidInput)
		}
		patch.DisplayName = &v
	}
	if patch.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*patch.Currency))
		if len(c) != 3 {
			return Profile{}, fmt.Errorf("%w: currency must be 3-letter code", ErrInvalidInput)
		}
		patch.Currency = &c
	}
	if patch.RateMin != nil && *patch.RateMin < 0 {
		return Profile{}, fmt.Errorf("%w: rate_min must be >= 0", ErrInvalidInput)
	}
	if patch.RateMax != nil && *patch.RateMax < 0 {
		return Profile{}, fmt.Errorf("%w: rate_max must be >= 0", ErrInvalidInput)
	}
	if patch.RateMin != nil && patch.RateMax != nil && *patch.RateMin > *patch.RateMax {
		return Profile{}, fmt.Errorf("%w: rate_min must be <= rate_max", ErrInvalidInput)
	}
	if patch.ContactEmail != nil {
		v := strings.TrimSpace(*patch.ContactEmail)
		if len(v) > 254 {
			return Profile{}, fmt.Errorf("%w: contact_email too long", ErrInvalidInput)
		}
		patch.ContactEmail = &v
	}
	if patch.ContactPhone != nil {
		v := strings.TrimSpace(*patch.ContactPhone)
		if len(v) > 64 {
			return Profile{}, fmt.Errorf("%w: contact_phone too long", ErrInvalidInput)
		}
		patch.ContactPhone = &v
	}

	// 2. Валидация categories (если переданы). Те же правила, что в SetCategories.
	var catCodes []string
	var catPrimary string
	if in.Categories != nil {
		catCodes = DedupStrings(in.Categories.Codes)
		catPrimary = in.Categories.Primary
		if len(catCodes) == 0 {
			return Profile{}, fmt.Errorf("%w: at least one category is required", ErrInvalidInput)
		}
		if catPrimary == "" {
			return Profile{}, fmt.Errorf("%w: primary category is required", ErrInvalidInput)
		}
		if !contains(catCodes, catPrimary) {
			return Profile{}, fmt.Errorf("%w: primary must be in codes", ErrInvalidInput)
		}
		valid, err := s.repo.ValidCategoryCodes(ctx, catCodes)
		if err != nil {
			return Profile{}, err
		}
		if len(valid) != len(catCodes) {
			return Profile{}, fmt.Errorf("%w: unknown category code", ErrInvalidInput)
		}
	}

	// 3. Валидация skills (если переданы). Те же правила, что в SetSkills.
	var skillIDs []uuid.UUID
	if in.Skills != nil {
		ids := make([]uuid.UUID, 0, len(in.Skills.SkillIDs))
		seen := map[uuid.UUID]struct{}{}
		for _, raw := range in.Skills.SkillIDs {
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
		skillIDs = ids
	}

	// 4. Одна транзакция: optimistic-lock проверяется один раз
	// (в PatchInTx или в LockProfileForUpdateInTx), дальше идём под row-lock.
	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if in.hasProfileFields() {
			if err := s.repo.PatchInTx(ctx, tx, userID, patch); err != nil {
				return err
			}
		} else {
			// patch-полей нет — просто взять row-lock и проверить версию.
			if err := s.repo.LockProfileForUpdateInTx(ctx, tx, userID, in.UpdatedAt); err != nil {
				return err
			}
		}
		if in.Categories != nil {
			if err := s.repo.ReplaceCategoriesInTx(ctx, tx, userID, catCodes, catPrimary); err != nil {
				return err
			}
		}
		if in.Skills != nil {
			if err := s.repo.ReplaceSkillsInTx(ctx, tx, userID, skillIDs); err != nil {
				return err
			}
		}
		// Любое изменение профиля (поля / категории / навыки) для уже-approved
		// спеца переводит его обратно в pending_review — изменения должен
		// увидеть админ до повторного попадания в каталог.
		// См. docs/SPECIALIST_MODERATION.md §2.
		bumped, err := s.repo.BumpModerationToPendingIfApprovedInTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()}); err != nil {
			return err
		}
		if bumped {
			return s.emitModerationPending(ctx, tx, userID, "content_changed")
		}
		return nil
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
	// Soft-gate: публиковать профиль можно только если юзер активен и email
	// подтверждён. Снять с публикации — без ограничений (юзер может всегда
	// «уйти», в т.ч. деактивированный — это очищает витрину).
	if published && s.verifier != nil {
		active, verified, err := s.verifier.CheckActiveVerified(ctx, userID)
		if err != nil {
			return Profile{}, fmt.Errorf("verify user: %w", err)
		}
		if !active {
			return Profile{}, ErrUserInactive
		}
		if !verified {
			return Profile{}, ErrEmailUnverified
		}
	}
	if published {
		// Хард-валидация перед публикацией. Базовая часть (production/bio/
		// display_name) обязательна всегда — без неё в каталог попадали
		// «пустые» карточки. LLM-проверка дополнительно, если есть checker.
		p, err := s.repo.Get(ctx, userID)
		if err != nil {
			return Profile{}, err
		}
		// Production XOR freelance: ровно одно должно быть выбрано. Иначе
		// в карточке отображается «непонятный статус» — фронту нечего
		// нарисовать (имя студии нет, фрилансером не объявлен).
		if p.ProductionID == nil && !p.IsFreelance {
			return Profile{}, fmt.Errorf("%w: выберите студию или отметьте «фрилансер»", ErrPublishIncomplete)
		}
		bio := strings.TrimSpace(p.Bio)
		name := strings.TrimSpace(p.DisplayName)
		if bio == "" {
			return Profile{}, fmt.Errorf("%w: bio is empty", ErrPublishIncomplete)
		}
		if name == "" {
			return Profile{}, fmt.Errorf("%w: display_name is empty", ErrPublishIncomplete)
		}
		if s.checker != nil && s.checker.Available() {
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
	}
	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		// R10: при публикации перечитываем профиль ВНУТРИ tx и валидируем
		// заново. Между Get (наверху) и SetPublishedInTx LLM-checker.Check
		// мог проработать секунды (а с adaptive thinking до десятков),
		// и юзер во втором табе успевал почистить bio/display_name или
		// переключить production. Без этой проверки публиковали профиль
		// с устаревшими значениями (пустой bio, нет студии).
		// Unpublish (published=false) валидацию пропускает — без условий.
		if published {
			if err := s.repo.RevalidatePublishInTx(ctx, tx, userID); err != nil {
				return err
			}
		}
		notify, err := s.repo.SetPublishedInTx(ctx, tx, userID, published)
		if err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			event, map[string]any{"user_id": userID.String()}); err != nil {
			return err
		}
		// Если спец встал в очередь модерации именно этим publish'ом —
		// шлём уведомление админу через n8n (см. docs/SPECIALIST_MODERATION.md §6).
		if notify {
			return s.emitModerationPending(ctx, tx, userID, "publish_requested")
		}
		return nil
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

	// 50 МБ — лимит для single PUT (legacy presign endpoint).
	portfolioMaxUploadBytes = 50 * 1024 * 1024
	portfolioUploadExpiry   = 15 * time.Minute

	// 200 МБ — лимит для multipart upload. 5 МБ part — минимум, который
	// принимает S3 (кроме последнего чанка), и удобный шаг для retry.
	portfolioMaxMultipartBytes = 200 * 1024 * 1024
	portfolioMultipartPartSize = 5 * 1024 * 1024

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
	if !IsHTTPURL(in.VideoURL) {
		return PortfolioItem{}, fmt.Errorf("%w: video_url must be http(s)", ErrInvalidInput)
	}
	if in.ThumbnailURL != "" && !IsHTTPURL(in.ThumbnailURL) {
		return PortfolioItem{}, fmt.Errorf("%w: thumbnail_url must be http(s)", ErrInvalidInput)
	}
	// data-sec D6: video/thumb должны лежать в нашем bucket'е. Раньше
	// принимался любой http(s) URL — спец мог опубликовать ссылку на
	// внешний phishing/malware-домен, и она бы уехала в OpenSearch-feed.
	// KeyFromURL возвращает "" если URL не из нашего PublicURL.
	if s.media != nil {
		if s.media.KeyFromURL(in.VideoURL) == "" {
			return PortfolioItem{}, fmt.Errorf("%w: video_url должен указывать на наше хранилище", ErrInvalidInput)
		}
		if in.ThumbnailURL != "" && s.media.KeyFromURL(in.ThumbnailURL) == "" {
			return PortfolioItem{}, fmt.Errorf("%w: thumbnail_url должен указывать на наше хранилище", ErrInvalidInput)
		}
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
	in.CategoryCodes = DedupStrings(in.CategoryCodes)
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

	// hard-лимит: 20 видео на спеца. Проверка ВНУТРИ транзакции с
	// row-lock'ом на specialist_profiles (через LockProfileForUpdateInTx) —
	// иначе двое параллельных INSERT'ов оба пройдут pre-check на 19 и
	// получат 21 видео. Lock сериализует их через одну строку профиля.
	var item PortfolioItem
	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.LockProfileForUpdateInTx(ctx, tx, userID, nil); err != nil {
			return err
		}
		n, err := s.repo.CountVideosInTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if n >= portfolioMaxVideosPerUser {
			return fmt.Errorf("%w: max %d videos", ErrInvalidInput, portfolioMaxVideosPerUser)
		}
		var txErr error
		item, txErr = s.repo.CreatePortfolioVideoInTx(ctx, tx, userID, in)
		if txErr != nil {
			return txErr
		}
		// Контент-апдейт у approved-спеца возвращает его в очередь модерации.
		bumped, err := s.repo.BumpModerationToPendingIfApprovedInTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		// Outbox-событие: воркер переиндексирует ES-документ спеца,
		// в т.ч. last_video_at — это критично для /feed ранжирования.
		if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()}); err != nil {
			return err
		}
		// portfolio.video_uploaded → транскод-пайплайн в воркере (см.
		// docs/VIDEO_TRANSCODING.md). Эмитим только если s3-ключ
		// резолвится из URL — для внешних ссылок (D6 их теперь не
		// принимает, но на всякий случай) транскода нет.
		if s.media != nil {
			if key := s.media.KeyFromURL(in.VideoURL); key != "" {
				if err := outbox.Emit(ctx, tx, outbox.AggregatePortfolio, item.ID.String(),
					outbox.EventPortfolioVideoUploaded, outbox.PortfolioVideoUploadedPayload{
						ItemID: item.ID.String(),
						UserID: userID.String(),
						S3Key:  key,
					}); err != nil {
					return err
				}
			}
		}
		if bumped {
			return s.emitModerationPending(ctx, tx, userID, "content_changed")
		}
		return nil
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
		bumped, err := s.repo.BumpModerationToPendingIfApprovedInTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()}); err != nil {
			return err
		}
		if bumped {
			return s.emitModerationPending(ctx, tx, userID, "content_changed")
		}
		return nil
	})
}

// SetPortfolioCategories — обновляет category_codes у видео-айтема.
// Категории должны быть подмножеством категорий профиля специалиста; пустой
// список разрешён (видео не попадёт ни под один категорийный фильтр).
// expectedUpdatedAt — optimistic-lock версия portfolio_items.updated_at,
// прислана фронтом из последнего GET. nil = без проверки.
func (s *Service) SetPortfolioCategories(ctx context.Context, userID, itemID uuid.UUID, codes []string, expectedUpdatedAt *time.Time) (PortfolioItem, error) {
	codes = DedupStrings(codes)
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
		bumped, err := s.repo.BumpModerationToPendingIfApprovedInTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()}); err != nil {
			return err
		}
		if bumped {
			return s.emitModerationPending(ctx, tx, userID, "content_changed")
		}
		return nil
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

	// Hard-cap 20 видео — soft-check здесь, чтобы не выдавать presigned URL
	// впустую если лимит уже достигнут. Настоящее enforcement делает
	// AddPortfolioVideo под row-lock'ом (см. там CountVideosInTx). Race
	// здесь не опасен: даже если 21-й URL выдан, POST /me/portfolio
	// откажет.
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

// StartPortfolioMultipart — инициирует S3 multipart upload для крупного
// видео. Возвращает upload_id, ключ и part_size, по которому фронт нарежет
// файл. Для каждой части фронт ходит за presigned PUT в PortfolioMultipartPartURL.
func (s *Service) StartPortfolioMultipart(
	ctx context.Context,
	userID uuid.UUID,
	in PortfolioMultipartStartInput,
) (PortfolioMultipartStartOutput, error) {
	if s.media == nil {
		return PortfolioMultipartStartOutput{}, errors.New("media storage not configured")
	}
	ext, ok := allowedUploadTypes[in.ContentType]
	if !ok {
		return PortfolioMultipartStartOutput{}, fmt.Errorf("%w: content_type must be video/mp4 or video/quicktime", ErrInvalidInput)
	}
	if in.SizeBytes <= 0 {
		return PortfolioMultipartStartOutput{}, fmt.Errorf("%w: size_bytes is required", ErrInvalidInput)
	}
	if in.SizeBytes > portfolioMaxMultipartBytes {
		return PortfolioMultipartStartOutput{}, fmt.Errorf("%w: file too large (max %d MB)", ErrInvalidInput, portfolioMaxMultipartBytes/(1024*1024))
	}

	// Тот же soft-check на лимит 20 видео, что у single-PUT.
	existing, err := s.repo.ListPortfolio(ctx, userID)
	if err != nil {
		return PortfolioMultipartStartOutput{}, err
	}
	videos := 0
	for _, it := range existing {
		if it.VideoURL != "" {
			videos++
		}
	}
	if videos >= portfolioMaxVideosPerUser {
		return PortfolioMultipartStartOutput{}, fmt.Errorf("%w: max %d videos", ErrInvalidInput, portfolioMaxVideosPerUser)
	}

	key := path.Join("portfolio", userID.String(), uuid.NewString()+ext)
	uploadID, err := s.media.NewMultipartUpload(ctx, key, in.ContentType)
	if err != nil {
		return PortfolioMultipartStartOutput{}, fmt.Errorf("new multipart: %w", err)
	}

	return PortfolioMultipartStartOutput{
		UploadID:  uploadID,
		Key:       key,
		PublicURL: s.media.PublicURL(key),
		PartSize:  portfolioMultipartPartSize,
	}, nil
}

// PortfolioMultipartPartURL — presigned PUT для одной части. Проверяем,
// что ключ принадлежит этому пользователю (защита от cross-user upload-id
// injection — мы не сохраняем own’ership в БД, опираемся на префикс ключа).
func (s *Service) PortfolioMultipartPartURL(
	ctx context.Context,
	userID uuid.UUID,
	in PortfolioMultipartPartURLInput,
) (PortfolioMultipartPartURLOutput, error) {
	if s.media == nil {
		return PortfolioMultipartPartURLOutput{}, errors.New("media storage not configured")
	}
	if err := assertOwnedKey(userID, in.Key); err != nil {
		return PortfolioMultipartPartURLOutput{}, err
	}
	if in.UploadID == "" {
		return PortfolioMultipartPartURLOutput{}, fmt.Errorf("%w: upload_id is required", ErrInvalidInput)
	}
	if in.PartNumber < 1 || in.PartNumber > 10000 {
		return PortfolioMultipartPartURLOutput{}, fmt.Errorf("%w: part_number must be in [1, 10000]", ErrInvalidInput)
	}
	u, err := s.media.PresignPart(ctx, in.Key, in.UploadID, in.PartNumber, portfolioUploadExpiry)
	if err != nil {
		return PortfolioMultipartPartURLOutput{}, fmt.Errorf("presign part: %w", err)
	}
	return PortfolioMultipartPartURLOutput{
		UploadURL: u,
		ExpiresIn: int(portfolioUploadExpiry.Seconds()),
	}, nil
}

// CompletePortfolioMultipart — финализирует multipart. После успеха
// фронт зовёт POST /me/portfolio с public_url (как и в single-PUT-флоу).
func (s *Service) CompletePortfolioMultipart(
	ctx context.Context,
	userID uuid.UUID,
	in PortfolioMultipartCompleteInput,
) error {
	if s.media == nil {
		return errors.New("media storage not configured")
	}
	if err := assertOwnedKey(userID, in.Key); err != nil {
		return err
	}
	if in.UploadID == "" {
		return fmt.Errorf("%w: upload_id is required", ErrInvalidInput)
	}
	if len(in.Parts) == 0 {
		return fmt.Errorf("%w: parts is empty", ErrInvalidInput)
	}
	parts := make([]MultipartPart, len(in.Parts))
	for i, p := range in.Parts {
		if p.PartNumber < 1 || p.PartNumber > 10000 {
			return fmt.Errorf("%w: part_number must be in [1, 10000]", ErrInvalidInput)
		}
		if p.ETag == "" {
			return fmt.Errorf("%w: etag is required for part %d", ErrInvalidInput, p.PartNumber)
		}
		parts[i] = MultipartPart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	return s.media.CompleteMultipart(ctx, in.Key, in.UploadID, parts)
}

// AbortPortfolioMultipart — отменяет multipart upload. Идемпотентно.
func (s *Service) AbortPortfolioMultipart(
	ctx context.Context,
	userID uuid.UUID,
	in PortfolioMultipartAbortInput,
) error {
	if s.media == nil {
		return errors.New("media storage not configured")
	}
	if err := assertOwnedKey(userID, in.Key); err != nil {
		return err
	}
	if in.UploadID == "" {
		return fmt.Errorf("%w: upload_id is required", ErrInvalidInput)
	}
	return s.media.AbortMultipart(ctx, in.Key, in.UploadID)
}

// assertOwnedKey — ключи имеют префикс portfolio/{user_id}/, без него
// клиент мог бы попросить presigned URL для чужого upload_id.
//
// data-sec D3: одной только HasPrefix-проверки недостаточно — ключ вида
// "portfolio/<my>/../<victim>/file.mp4" префиксу удовлетворяет, но после
// S3-нормализации указывает на чужой объект. Поэтому дополнительно:
//   1) запрещаем "..", "//" и начальный "/" в сыром ключе (раннее
//      отсечение очевидного traversal),
//   2) сверяем path.Clean(key) с исходником — любая нормализация значит
//      попытку обмана.
func assertOwnedKey(userID uuid.UUID, key string) error {
	if key == "" ||
		strings.HasPrefix(key, "/") ||
		strings.Contains(key, "..") ||
		strings.Contains(key, "//") {
		return fmt.Errorf("%w: invalid key", ErrInvalidInput)
	}
	if path.Clean(key) != key {
		return fmt.Errorf("%w: invalid key", ErrInvalidInput)
	}
	prefix := "portfolio/" + userID.String() + "/"
	if !strings.HasPrefix(key, prefix) {
		return fmt.Errorf("%w: key does not belong to user", ErrInvalidInput)
	}
	return nil
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

func IsHTTPURL(s string) bool {
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

// NormalizedProduction — результат XOR-нормализации production/freelance.
// Используется и сервисом (заполняет одноимённые поля PatchInput), и тестами.
type NormalizedProduction struct {
	SetProduction  bool
	ProductionID   *uuid.UUID
	SetIsFreelance bool
	IsFreelance    bool
}

// NormalizeProduction — XOR между production_id и is_freelance.
// Семантика:
//   productionID = "uuid" → выбрать (авто-снимает freelance);
//   productionID = ""    → снять продакшен (SET NULL);
//   productionID = nil   → не трогать.
//   isFreelance  = &true → стать фрилансером (авто-снимает production);
//   isFreelance  = &false → выключить freelance;
//   isFreelance  = nil   → не трогать.
// Конфликт productionID=valid + isFreelance=true → ErrInvalidInput.
// lookup может быть nil — тогда активность не проверяется, и FK ловит
// только несуществующий id.
func NormalizeProduction(ctx context.Context, productionID *string, isFreelance *bool, lookup ProductionLookup) (NormalizedProduction, error) {
	var out NormalizedProduction
	if productionID != nil {
		out.SetProduction = true
		raw := strings.TrimSpace(*productionID)
		if raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				return NormalizedProduction{}, fmt.Errorf("%w: production_id is not a uuid", ErrInvalidInput)
			}
			if lookup != nil {
				ok, err := lookup.IsActiveProduction(ctx, id)
				if err != nil {
					return NormalizedProduction{}, fmt.Errorf("lookup production: %w", err)
				}
				if !ok {
					return NormalizedProduction{}, fmt.Errorf("%w: production not found or inactive", ErrInvalidInput)
				}
			}
			out.ProductionID = &id
			out.SetIsFreelance = true
			out.IsFreelance = false
		}
	}
	if isFreelance != nil {
		if *isFreelance && out.ProductionID != nil {
			return NormalizedProduction{}, fmt.Errorf("%w: production_id and is_freelance=true are mutually exclusive", ErrInvalidInput)
		}
		out.SetIsFreelance = true
		out.IsFreelance = *isFreelance
		if *isFreelance {
			out.SetProduction = true
			out.ProductionID = nil
		}
	}
	return out, nil
}

func DedupStrings(in []string) []string {
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

// emitModerationPending — эмит outbox-события «спец встал в очередь
// модерации». Воркер пробрасывает его в n8n, чтобы админ получил
// telegram/email и пошёл одобрять. См. docs/SPECIALIST_MODERATION.md §6.
//
// Никогда не ломаем основной transaction если notify info не загружается —
// уведомление это нотификация, не источник истины. Поэтому: если профиль
// исчез между UPDATE'ом и SELECT'ом (что почти невозможно), молча скипаем.
func (s *Service) emitModerationPending(ctx context.Context, tx pgx.Tx, userID uuid.UUID, reason string) error {
	email, displayName, err := s.repo.LoadModerationNotifyInfo(ctx, tx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return outbox.Emit(ctx, tx, outbox.AggregateModeration, userID.String(),
		outbox.EventModerationSpecialistPending,
		outbox.ModerationSpecialistPendingPayload{
			UserID:      userID.String(),
			Email:       email,
			DisplayName: displayName,
			Reason:      reason,
		})
}
