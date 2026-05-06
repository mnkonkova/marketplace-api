package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"marketpclce/internal/auth"
	"marketpclce/internal/catalog"
	"marketpclce/internal/clarify"
	"marketpclce/internal/config"
	"marketpclce/internal/feed"
	"marketpclce/internal/httpapi"
	"marketpclce/internal/leads"
	"marketpclce/internal/llm"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
	"marketpclce/internal/platform/redisx"
	"marketpclce/internal/platform/s3"
	"marketpclce/internal/profilecheck"
	"marketpclce/internal/profiles"
	"marketpclce/internal/ratelimit"
	"marketpclce/internal/search"
	"marketpclce/internal/summarize"
)

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
	authHandler := auth.NewHandler(authSvc)

	catalogRepo := catalog.NewRepo(pool)
	catalogHandler := catalog.NewHandler(catalogRepo)

	profilesRepo := profiles.NewRepo(pool)
	profilesSvc := profiles.NewService(profilesRepo)
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

	feedRepo := feed.NewRepo(pool)
	feedSvc := feed.NewService(searchSvc, feedRepo)
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

	clarifySvc := clarify.NewService(llmClient, 1024, cfg.LLMEffort)
	clarifyHandler := clarify.NewHandler(clarifySvc)

	profileCheckSvc := profilecheck.NewService(llmClient, 1024, cfg.LLMEffort)
	profileCheckHandler := profilecheck.NewHandler(profileCheckSvc, primaryCategoryLookup{repo: profilesRepo})
	profilesSvc.WithProfileChecker(profileCheckerAdapter{svc: profileCheckSvc})

	leadsRepo := leads.NewRepo(pool)
	leadsSvc := leads.NewService(leadsRepo)
	leadsHandler := leads.NewHandler(leadsSvc)

	var summarizeCache *summarize.Cache
	var limiter *ratelimit.Limiter
	if rdb != nil {
		summarizeCache = summarize.NewCache(rdb, cfg.SummarizeCacheTTL)
		limiter = ratelimit.New(rdb)
	}
	summarizeHandler := summarize.NewHandler(summarizeSvc, summarize.HandlerConfig{
		Search:  searchSvc,
		Cache:   summarizeCache,
		Limiter: limiter,
		RLWindows: []ratelimit.Window{
			{Limit: cfg.RateSummarizePerMin, Period: time.Minute},
			{Limit: cfg.RateSummarizePerHour, Period: time.Hour},
		},
	})

	router := httpapi.NewRouter(httpapi.Deps{
		Logger:       logger,
		HealthDB:     pool,
		TokenIssuer:  tokenIssuer,
		Auth:         authHandler,
		Catalog:      catalogHandler,
		Profiles:     profilesHandler,
		ProfileCheck: profileCheckHandler,
		Search:       searchHandler,
		Feed:         feedHandler,
		Summarize:    summarizeHandler,
		Clarify:      clarifyHandler,
		Leads:        leadsHandler,
		WebDir:       cfg.WebDir,
		Limiter:     limiter,
		ReadWindows: []ratelimit.Window{
			{Limit: cfg.RateReadPerMin, Period: time.Minute},
			{Limit: cfg.RateReadPerHour, Period: time.Hour},
		},
		LeadsWindows: []ratelimit.Window{
			{Limit: cfg.RateLeadsPerMin, Period: time.Minute},
			{Limit: cfg.RateLeadsPerHour, Period: time.Hour},
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

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl, AddSource: false}))
}
