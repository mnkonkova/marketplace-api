package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	AppEnv string `env:"APP_ENV" envDefault:"local"`

	HTTPAddr            string        `env:"HTTP_ADDR" envDefault:":8080"`
	HTTPReadTimeout     time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"30s"`
	HTTPWriteTimeout    time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"120s"`
	HTTPShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"15s"`

	DatabaseURL      string `env:"DATABASE_URL,required"`
	DatabaseMaxConns int32  `env:"DATABASE_MAX_CONNS" envDefault:"25"`

	OpenSearchURL             string `env:"OPENSEARCH_URL" envDefault:"http://localhost:9200"`
	OpenSearchIndexProfile    string `env:"OPENSEARCH_INDEX_SPECIALISTS" envDefault:"specialists"`
	OpenSearchIndexFeedVideos string `env:"OPENSEARCH_INDEX_FEED_VIDEOS" envDefault:"feed_videos"`

	RedisAddr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	RedisPassword string `env:"REDIS_PASSWORD"`
	RedisDB       int    `env:"REDIS_DB" envDefault:"0"`

	S3Endpoint  string `env:"S3_ENDPOINT" envDefault:"http://localhost:9000"`
	S3AccessKey string `env:"S3_ACCESS_KEY"`
	S3SecretKey string `env:"S3_SECRET_KEY"`
	S3Bucket    string `env:"S3_BUCKET" envDefault:"marketpclce"`
	S3Region    string `env:"S3_REGION" envDefault:"us-east-1"`
	S3UseSSL    bool   `env:"S3_USE_SSL" envDefault:"false"`
	// Опциональный публичный домен для отдачи объектов: если задан, public_url
	// собирается как `${S3_PUBLIC_URL}/${key}` (CNAME, CDN). Иначе —
	// `${S3_ENDPOINT}/${S3_BUCKET}/${key}` (path-style на YC по умолчанию).
	S3PublicURL string `env:"S3_PUBLIC_URL"`

	LLMProvider  string        `env:"LLM_PROVIDER" envDefault:"anthropic"`
	LLMAPIKey    string        `env:"LLM_API_KEY"`
	LLMModel     string        `env:"LLM_MODEL"`
	LLMBaseURL   string        `env:"LLM_BASE_URL"`
	LLMMaxTokens int           `env:"LLM_MAX_TOKENS" envDefault:"2048"`
	LLMTimeout   time.Duration `env:"LLM_TIMEOUT" envDefault:"60s"`
	LLMEffort    string        `env:"LLM_EFFORT" envDefault:"medium"`

	JWTSecret     string        `env:"JWT_SECRET,required"`
	JWTAccessTTL  time.Duration `env:"JWT_ACCESS_TTL" envDefault:"30m"`
	JWTRefreshTTL time.Duration `env:"JWT_REFRESH_TTL" envDefault:"168h"`

	// Email verification (Unisender Go). Без UNISENDER_API_KEY — отправка
	// отключена: register работает, но письма не уходят, в логах warn.
	// APP_BASE_URL нужен воркеру для сборки verify-ссылки (у воркера нет
	// HTTP-контекста, на dev/staging/prod разный URL).
	UnisenderAPIKey      string        `env:"UNISENDER_API_KEY"`
	UnisenderAPIBaseURL  string        `env:"UNISENDER_API_BASE_URL" envDefault:"https://go1.unisender.ru/ru/transactional/api/v1"`
	UnisenderFromEmail   string        `env:"UNISENDER_FROM_EMAIL"`
	UnisenderFromName    string        `env:"UNISENDER_FROM_NAME" envDefault:"marketpclce"`
	AppBaseURL           string        `env:"APP_BASE_URL" envDefault:"http://localhost:5173"`
	EmailVerifyTokenTTL  time.Duration `env:"EMAIL_VERIFY_TOKEN_TTL" envDefault:"24h"`
	RateEmailResendPer   time.Duration `env:"RATE_EMAIL_RESEND_PER" envDefault:"60s"`
	// EmailVerificationDisabled — выключает весь soft-gate целиком: юзер при
	// регистрации сразу email_verified=true, publish/leads проходят без
	// проверки, resend/verify становятся no-op'ами. Для локального запуска
	// без Unisender. В проде .env.prod не должен ставить true.
	EmailVerificationDisabled bool `env:"EMAIL_VERIFICATION_DISABLED" envDefault:"false"`

	SummarizeCacheTTL    time.Duration `env:"SUMMARIZE_CACHE_TTL" envDefault:"10m"`
	FeedCacheTTL         time.Duration `env:"FEED_CACHE_TTL" envDefault:"30s"`
	RateSummarizePerMin  int           `env:"RATE_SUMMARIZE_PER_MIN" envDefault:"5"`
	RateSummarizePerHour int           `env:"RATE_SUMMARIZE_PER_HOUR" envDefault:"30"`
	RateClarifyPerMin    int           `env:"RATE_CLARIFY_PER_MIN" envDefault:"15"`
	RateClarifyPerHour   int           `env:"RATE_CLARIFY_PER_HOUR" envDefault:"120"`
	RateReadPerMin       int           `env:"RATE_READ_PER_MIN" envDefault:"60"`
	RateReadPerHour      int           `env:"RATE_READ_PER_HOUR" envDefault:"600"`
	RateLeadsPerMin      int           `env:"RATE_LEADS_PER_MIN" envDefault:"5"`
	RateLeadsPerHour     int           `env:"RATE_LEADS_PER_HOUR" envDefault:"20"`
	// Лимиты на /auth/* — анти-брутфорс по логину и анти-флуд по регистрации.
	// Считается по IP. На login достаточно жёстко: 10 попыток/мин ловит
	// автоматику, но не мешает живому юзеру опечататься 2-3 раза.
	RateAuthPerMin       int           `env:"RATE_AUTH_PER_MIN" envDefault:"10"`
	RateAuthPerHour      int           `env:"RATE_AUTH_PER_HOUR" envDefault:"60"`

	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// CORSOrigins — список разрешённых origin'ов через запятую
	// (например "http://localhost:5173,https://app.example.com"). Пусто —
	// CORS-заголовки не выставляются (фронт на том же домене / прокси).
	CORSOrigins []string `env:"CORS_ORIGINS" envSeparator:","`
}

func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
