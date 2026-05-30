package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"marketpclce/internal/auth"
	"marketpclce/internal/catalog"
	"marketpclce/internal/clarify"
	"marketpclce/internal/config"
	"marketpclce/internal/feed"
	"marketpclce/internal/httpapi"
	"marketpclce/internal/invites"
	"marketpclce/internal/leads"
	"marketpclce/internal/llm"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
	"marketpclce/internal/platform/redisx"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/productions"
	"marketpclce/internal/profilecheck"
	"marketpclce/internal/profiles"
	"marketpclce/internal/projects"
	"marketpclce/internal/ratelimit"
	"marketpclce/internal/reviews"
	"marketpclce/internal/search"
	"marketpclce/internal/summarize"

	// Сгенерённый swaggo-пакет (`make swag`) — регистрирует OpenAPI spec в init(),
	// чтобы http-swagger мог отдать /swagger/doc.json.
	_ "marketpclce/docs/swagger"
)

// @title           marketpclce API
// @version         1.0
// @description     Discovery-маркетплейс специалистов: каталог, поиск, лиды, отзывы.
// @BasePath        /api/v1
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(rootCtx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	esClient := es.New(cfg.OpenSearchURL)
	if err := esClient.EnsureIndex(rootCtx, cfg.OpenSearchIndexProfile, search.IndexMapping()); err != nil {
		slog.Warn("ensure index (continuing)", "err", err)
	}
	if err := esClient.EnsureIndex(rootCtx, cfg.OpenSearchIndexFeedVideos, search.FeedVideoMapping()); err != nil {
		slog.Warn("ensure feed_videos index (continuing)", "err", err)
	}

	rdb, err := redisx.New(rootCtx, redisx.Config{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		slog.Warn("redis connect failed (rate-limit and cache disabled)", "err", err)
	}
	defer func() {
		if rdb != nil {
			_ = rdb.Close()
		}
	}()

	tokenIssuer := auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTAccessTTL, cfg.JWTRefreshTTL)
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, tokenIssuer)
	// Resend-cooldown — простая обёртка над общим ratelimit.Limiter
	// (limit=1, period=cfg.RateEmailResendPer). Без Redis cooldown nil,
	// resend проходит без ограничений (это намеренно: не валим фичу
	// если Redis недоступен, только теряем защиту от спама ящика).
	var resendCooldown auth.ResendCooldown
	if rdb != nil {
		resendCooldown = emailResendCooldown{
			limiter: ratelimit.New(rdb),
			period:  cfg.RateEmailResendPer,
		}
	}
	authSvc.WithEmailVerification(cfg.EmailVerifyTokenTTL, cfg.AppBaseURL, resendCooldown, cfg.EmailVerificationDisabled)
	if cfg.EmailVerificationDisabled {
		slog.Warn("email verification DISABLED — users auto-verified on register; soft-gate skipped (для прода держать выключенным!)")
	}
	authHandler := auth.NewHandler(authSvc)

	catalogRepo := catalog.NewRepo(pool)
	catalogHandler := catalog.NewHandler(catalogRepo)

	profilesRepo := profiles.NewRepo(pool)
	profilesSvc := profiles.NewService(profilesRepo).WithEmailVerifier(authSvc)
	profilesHandler := profiles.NewHandler(profilesSvc)

	// S3 — опционально: без ключей API стартует без upload-функционала.
	// Ручка POST /me/portfolio/upload-url вернёт 503 storage_disabled.
	if cfg.S3AccessKey != "" && cfg.S3SecretKey != "" {
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
			slog.Warn("s3 disabled", "err", err)
		} else {
			profilesSvc.WithMediaStorage(s3Client)
			slog.Info("s3 ready", "bucket", s3Client.Bucket(), "endpoint", cfg.S3Endpoint)
		}
	} else {
		slog.Info("s3 disabled (no credentials)")
	}

	searchSvc := search.NewService(esClient, cfg.OpenSearchIndexProfile)
	searchHandler := search.NewHandler(searchSvc)

	feedSvc := feed.NewService(esClient, cfg.OpenSearchIndexFeedVideos)
	if rdb != nil {
		feedSvc.WithCache(feed.NewCache(rdb, cfg.FeedCacheTTL))
	}
	feedHandler := feed.NewHandler(feedSvc)

	llmClient, err := llm.NewProvider(llm.ProviderConfig{
		Name:    cfg.LLMProvider,
		BaseURL: cfg.LLMBaseURL,
		APIKey:  cfg.LLMAPIKey,
		Model:   cfg.LLMModel,
		Timeout: cfg.LLMTimeout,
	})
	if err != nil {
		slog.Error("llm provider", "err", err)
		os.Exit(1)
	}
	slog.Info("llm provider ready", "provider", cfg.LLMProvider, "model", llmClient.Model(), "has_key", llmClient.HasKey())
	summarizeSvc := summarize.NewService(searchSvc, llmClient, cfg.LLMMaxTokens, cfg.LLMEffort)

	clarifySvc := clarify.NewService(llmClient, 1024, cfg.LLMEffort).
		WithCategoryLister(clarifyCategoryAdapter{repo: catalogRepo}).
		WithSkillLister(clarifySkillAdapter{repo: catalogRepo})
	clarifyHandler := clarify.NewHandler(clarifySvc)

	profileCheckSvc := profilecheck.NewService(llmClient, 1024, cfg.LLMEffort)
	profileCheckHandler := profilecheck.NewHandler(profileCheckSvc, primaryCategoryLookup{repo: profilesRepo})
	profilesSvc.WithProfileChecker(profileCheckerAdapter{svc: profileCheckSvc})

	leadsRepo := leads.NewRepo(pool)
	leadsSvc := leads.NewService(leadsRepo).WithEmailVerifier(authSvc)
	leadsHandler := leads.NewHandler(leadsSvc)

	reviewsRepo := reviews.NewRepo(pool)
	reviewsSvc := reviews.NewService(reviewsRepo)
	reviewsHandler := reviews.NewHandler(reviewsSvc)

	productionsRepo := productions.NewRepo(pool)
	productionsSvc := productions.NewService(productionsRepo)
	productionsHandler := productions.NewHandler(productionsSvc)
	// Профиль валидирует production_id через справочник.
	profilesSvc.WithProductionRegistry(productionsSvc)

	projectsRepo := projects.NewRepo(pool)
	projectsSvc := projects.NewService(projectsRepo).
		WithUserDirectory(projectsUserDirectory{authRepo: authRepo}).
		WithReviewWriter(projectsReviewWriter{reviewsRepo: reviewsRepo}).
		WithRecipientAcceptor(projectsRecipientAcceptor{leadsSvc: leadsSvc})
	projectsHandler := projects.NewHandler(projectsSvc)

	invitesRepo := invites.NewRepo(pool)
	invitesSvc := invites.NewService(invitesRepo).
		WithUserDirectory(projectsUserDirectory{authRepo: authRepo}).
		WithAppBaseURL(cfg.AppBaseURL)
	invitesHandler := invites.NewHandler(invitesSvc, invitesClientCreator{pool: pool})
	invitesRedeem := invites.NewRedeemHandler(invitesSvc, tokenIssuer)

	var summarizeCache *summarize.Cache
	var limiter *ratelimit.Limiter
	if rdb != nil {
		summarizeCache = summarize.NewCache(rdb, cfg.SummarizeCacheTTL)
		limiter = ratelimit.New(rdb)
	}
	summarizeHandler := summarize.NewHandler(summarizeSvc, summarize.HandlerConfig{
		Search: searchSvc,
		Cache:  summarizeCache,
	})

	router := httpapi.NewRouter(httpapi.Deps{
		Logger:        logger,
		HealthDB:      pool,
		TokenIssuer:   tokenIssuer,
		Auth:          authHandler,
		Catalog:       catalogHandler,
		Profiles:      profilesHandler,
		ProfileCheck:  profileCheckHandler,
		Search:        searchHandler,
		Feed:          feedHandler,
		Summarize:     summarizeHandler,
		Clarify:       clarifyHandler,
		Leads:         leadsHandler,
		Reviews:       reviewsHandler,
		Productions:   productionsHandler,
		Projects:      projectsHandler,
		Invites:       invitesHandler,
		InvitesRedeem: invitesRedeem,
		RoleLookup:    authRepo,
		CORSOrigins:   cfg.CORSOrigins,
		Limiter:       limiter,
		ReadWindows: []ratelimit.Window{
			{Limit: cfg.RateReadPerMin, Period: time.Minute},
			{Limit: cfg.RateReadPerHour, Period: time.Hour},
		},
		LeadsWindows: []ratelimit.Window{
			{Limit: cfg.RateLeadsPerMin, Period: time.Minute},
			{Limit: cfg.RateLeadsPerHour, Period: time.Hour},
		},
		ClarifyWindows: []ratelimit.Window{
			{Limit: cfg.RateClarifyPerMin, Period: time.Minute},
			{Limit: cfg.RateClarifyPerHour, Period: time.Hour},
		},
		AuthWindows: []ratelimit.Window{
			{Limit: cfg.RateAuthPerMin, Period: time.Minute},
			{Limit: cfg.RateAuthPerHour, Period: time.Hour},
		},
		SummarizeWindows: []ratelimit.Window{
			{Limit: cfg.RateSummarizePerMin, Period: time.Minute},
			{Limit: cfg.RateSummarizePerHour, Period: time.Hour},
		},
	})

	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      router,
		ReadTimeout:  cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout,
	}

	go func() {
		slog.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http serve", "err", err)
			stop()
		}
	}()

	<-rootCtx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown", "err", err)
	}
	slog.Info("bye")
}

type profileCheckerAdapter struct{ svc *profilecheck.Service }

func (a profileCheckerAdapter) Available() bool { return a.svc.Available() }

func (a profileCheckerAdapter) Check(ctx context.Context, in profiles.CheckInput) (profiles.CheckResult, error) {
	res, err := a.svc.Check(ctx, profilecheck.Input{
		Bio:                  in.Bio,
		DisplayName:          in.DisplayName,
		PrimaryCategory:      in.PrimaryCategory,
		PrimaryCategoryTitle: in.PrimaryCategoryTitle,
	})
	if err != nil {
		return profiles.CheckResult{}, err
	}
	return profiles.CheckResult{
		OK:   res.OK,
		Bio:  profiles.PartResult(res.Bio),
		Name: profiles.PartResult(res.Name),
	}, nil
}

// emailResendCooldown — адаптер ratelimit.Limiter под auth.ResendCooldown.
// Окно (limit=1, period=cfg.RateEmailResendPer) — «не чаще раза в N
// секунд». Acquire возвращает false если cooldown ещё активен.
type emailResendCooldown struct {
	limiter *ratelimit.Limiter
	period  time.Duration
}

func (c emailResendCooldown) Acquire(ctx context.Context, key string) (bool, error) {
	err := c.limiter.Allow(ctx, "email-resend", key, []ratelimit.Window{{Limit: 1, Period: c.period}})
	if _, ok := ratelimit.IsRateLimited(err); ok {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// clarifyCategoryAdapter — мост между catalog.Repo и clarify.CategoryLister.
// Тянет актуальный список категорий из БД, чтобы LLM-промпт в /clarify
// видел свежие коды (включая ai_creator и прочие, добавленные после
// первой версии prompt.go). Кеш TTL — на стороне clarify.Service.
type clarifyCategoryAdapter struct{ repo *catalog.Repo }

func (a clarifyCategoryAdapter) ListCategoriesForPrompt(ctx context.Context) ([]clarify.CategoryRef, error) {
	cats, err := a.repo.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]clarify.CategoryRef, 0, len(cats))
	for _, c := range cats {
		out = append(out, clarify.CategoryRef{
			Code:        c.Code,
			Title:       c.Title,
			Description: c.Description,
		})
	}
	return out, nil
}

// clarifySkillAdapter — мост к clarify.SkillLister. Когда category пустая,
// отдаём весь словарь (нужен для первой реплики, до выбора категории).
// Когда выбрана — фильтруем по skill_categories и докидываем все платформы
// сверху, потому что reels/tiktok/... — кросс-категорийный фасет, который
// LLM должен узнавать в любой ситуации.
type clarifySkillAdapter struct{ repo *catalog.Repo }

func (a clarifySkillAdapter) ListSkillsForPrompt(ctx context.Context, category string) ([]clarify.SkillRef, error) {
	skills, err := a.repo.ListSkills(ctx, catalog.SkillFilter{Category: category})
	if err != nil {
		return nil, err
	}
	out := make([]clarify.SkillRef, 0, len(skills))
	for _, s := range skills {
		out = append(out, clarify.SkillRef{Slug: s.Slug, Title: s.Title})
	}
	if category != "" {
		// Платформы не входят в skill_categories (отдельный фасет), но в
		// промпте они нужны всегда — иначе LLM не сможет вернуть skill=reels,
		// когда категория уже зафиксирована.
		platforms, err := a.repo.ListSkills(ctx, catalog.SkillFilter{Kind: "platform"})
		if err != nil {
			return nil, err
		}
		for _, p := range platforms {
			out = append(out, clarify.SkillRef{Slug: p.Slug, Title: p.Title})
		}
	}
	return out, nil
}

type primaryCategoryLookup struct{ repo *profiles.Repo }

func (l primaryCategoryLookup) PrimaryCategory(ctx context.Context, userID uuid.UUID) (string, string, error) {
	p, err := l.repo.Get(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if p.PrimaryCategory == "" {
		return "", "", nil
	}
	title, _ := l.repo.CategoryTitle(ctx, p.PrimaryCategory)
	return p.PrimaryCategory, title, nil
}

// projectsReviewWriter — мост к reviews.Repo для submit_review.
// Использует reviews.Repo.Create напрямую (так как нам нужен project_id,
// которого нет в reviews.Service.CreateInput); добавляет project_id
// через сырой SQL после Create.
type projectsReviewWriter struct{ reviewsRepo *reviews.Repo }

func (p projectsReviewWriter) CreateProjectReview(ctx context.Context, in projects.ProjectReviewInput) (uuid.UUID, error) {
	id, err := p.reviewsRepo.Create(ctx, reviews.CreateInput{
		LeadID:       in.LeadID,
		AuthorUserID: in.AuthorUserID,
		AuthorName:   in.AuthorName,
		TargetUserID: in.TargetUserID,
		Rating:       in.Rating,
		Text:         in.Text,
	})
	if err != nil {
		return uuid.Nil, err
	}
	// Reviews.Repo.Create НЕ заполняет project_id (он был добавлен миграцией
	// 00011 после того, как Repo был написан). Заполняем отдельным UPDATE.
	if _, err := p.reviewsRepo.Pool().Exec(ctx,
		`UPDATE reviews SET project_id = $1 WHERE id = $2`,
		in.ProjectID, id,
	); err != nil {
		// Review создан — UPDATE упал. На MVP терпимо: project_id NULL
		// не ломает выдачу; админ может вручную допроставить через Directus.
		return id, nil
	}
	return id, nil
}

// projectsRecipientAcceptor — мост к leads.Service для админ-кнопки
// «акцептить за свой продакшен» в Directus.
type projectsRecipientAcceptor struct{ leadsSvc *leads.Service }

func (p projectsRecipientAcceptor) AcceptRecipient(ctx context.Context, leadID, specialistID uuid.UUID) error {
	// leads.Service.UpdateRecipientStatus принимает expectedUpdatedAt
	// для optimistic-lock; для админской кнопки мы не знаем версию —
	// передаём nil, действие безусловное (под admin-ролью).
	return p.leadsSvc.UpdateRecipientStatus(ctx, leadID, specialistID, "accepted", nil)
}

// invitesClientCreator — INSERT нового user'а (или возврат существующего)
// для POST /admin/users. Создаёт user с role=client/kind=client и dummy
// password_hash, чтобы прямой логин был невозможен — только через
// redeem invite. email_verified_at=NULL до redeem'а.
type invitesClientCreator struct{ pool *pgxpool.Pool }

func (c invitesClientCreator) CreateClient(ctx context.Context, email, displayName string) (uuid.UUID, bool, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return uuid.Nil, false, fmt.Errorf("email required")
	}

	// 1. Проверяем существование.
	var existing uuid.UUID
	err := c.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1 LIMIT 1`, email).Scan(&existing)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("lookup user: %w", err)
	}

	// 2. Создаём нового. dummy hash (никогда не матчится при login).
	id := uuid.New()
	const dummyHash = "!magic-link-only-no-password-login!"
	_, err = c.pool.Exec(ctx, `
INSERT INTO users (id, email, password_hash, kind, role)
VALUES ($1, $2, $3, 'client', 'client')`,
		id, email, dummyHash)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("insert user: %w", err)
	}

	// Если передано имя — заведём минимальный specialist_profile-стаб?
	// Нет — клиенты не имеют specialist_profile. Имя пока теряется
	// (можно положить в notes на проектах). Для MVP это OK.
	_ = displayName

	return id, false, nil
}

// projectsUserDirectory — мост от projects.UserDirectory к auth.Repo.
// Возвращает email + display_name (или email вместо имени, если профиля
// нет — для kind=client без specialist_profile). Используется для
// денормализации в outbox payload (см. projects/events.go).
type projectsUserDirectory struct{ authRepo *auth.Repo }

func (p projectsUserDirectory) GetEmailAndName(ctx context.Context, userID uuid.UUID) (string, string, error) {
	u, err := p.authRepo.FindByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	email := ""
	if u.Email != nil {
		email = *u.Email
	}
	name, _ := p.authRepo.GetDisplayName(ctx, userID)
	if name == "" {
		name = email
	}
	return email, name, nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl, AddSource: false}))
}
