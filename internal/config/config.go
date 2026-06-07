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

	// Email verification: API эмитит email.verify_send / email.password_reset_send
	// в outbox, worker дёргает n8n webhook (см. N8N_EMAIL_WEBHOOK_URL ниже).
	// n8n рендерит письмо и шлёт через свой провайдер (UniSender, SMTP, etc).
	// APP_BASE_URL нужен воркеру для сборки verify-ссылки (у воркера нет
	// HTTP-контекста, на dev/staging/prod разный URL) — попадает в payload.
	AppBaseURL          string        `env:"APP_BASE_URL" envDefault:"http://localhost:5173"`
	EmailVerifyTokenTTL time.Duration `env:"EMAIL_VERIFY_TOKEN_TTL" envDefault:"24h"`
	RateEmailResendPer  time.Duration `env:"RATE_EMAIL_RESEND_PER" envDefault:"60s"`
	// EmailVerificationDisabled — выключает весь soft-gate целиком: юзер при
	// регистрации сразу email_verified=true, publish/leads проходят без
	// проверки, resend/verify становятся no-op'ами. Для локального запуска
	// без n8n. В проде .env.prod не должен ставить true.
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

	// CRM endpoints (/me/projects, /me/specialist, /manager, /admin).
	// Лимит per user+ip — менеджеры/админы пишут плотно, но не должно быть
	// возможности устроить шторм action-endpoint-ов (start/skip/approve в
	// бесконечном цикле, например).
	RateCRMPerMin  int `env:"RATE_CRM_PER_MIN" envDefault:"120"`
	RateCRMPerHour int `env:"RATE_CRM_PER_HOUR" envDefault:"3000"`

	// Outbox-воркер. MaxAttempts/BackoffCap определяют поведение ретраев и
	// порог для DLQ (dead_at). Retention/CleanupInterval — TTL на обработанные
	// записи (dead-записи cleanup не трогает). WorkerMetricsAddr — отдельный
	// HTTP-listener у воркера для /metrics (alloy скрейпит worker:9090/metrics).
	OutboxMaxAttempts     int           `env:"OUTBOX_MAX_ATTEMPTS" envDefault:"10"`
	OutboxBackoffCap      time.Duration `env:"OUTBOX_BACKOFF_CAP" envDefault:"10m"`
	OutboxRetention       time.Duration `env:"OUTBOX_RETENTION" envDefault:"168h"`
	OutboxCleanupInterval time.Duration `env:"OUTBOX_CLEANUP_INTERVAL" envDefault:"1h"`
	WorkerMetricsAddr     string        `env:"WORKER_METRICS_ADDR" envDefault:":9090"`

	// Транскод-пайплайн preview-видео (см. docs/VIDEO_TRANSCODING.md).
	// FFmpegPath пустой → exec.LookPath("ffmpeg") (берётся первый из PATH).
	// Если ffmpeg не найден на старте воркера — обработчик
	// portfolio.video_uploaded стартует как no-op (логирует и квитирует),
	// чтобы воркер запускался и в dev-окружении без ffmpeg.
	FFmpegPath        string        `env:"FFMPEG_PATH" envDefault:""`
	TranscodeTimeout  time.Duration `env:"TRANSCODE_TIMEOUT" envDefault:"90s"`
	TranscodeTempDir  string        `env:"TRANSCODE_TEMP_DIR" envDefault:"/tmp/transcode"`

	// CRM v5: review-шаг ставится в waiting_client с deadline=now+ReviewDeadline.
	// По истечении worker переводит шаг в skipped. Дефолт 7 дней.
	ReviewDeadline      time.Duration `env:"REVIEW_DEADLINE" envDefault:"168h"`
	ReviewCheckInterval time.Duration `env:"REVIEW_CHECK_INTERVAL" envDefault:"1h"`

	// ProjectRetention — через сколько после completed_at done-проект
	// физически удаляется из БД (вместе со снэпшотом стадий/шагов,
	// событиями, комментариями — все каскады). По умолчанию 7 дней:
	// клиент ещё может вернуться, скачать что-то, потом убираем.
	ProjectRetention time.Duration `env:"PROJECT_RETENTION" envDefault:"168h"`
	// ProjectCancelledRetention — для cancelled больше: история диспа может
	// понадобиться разобраться с клиентом, поэтому 30 дней по умолчанию.
	ProjectCancelledRetention time.Duration `env:"PROJECT_CANCELLED_RETENTION" envDefault:"720h"`
	ProjectCleanupInterval    time.Duration `env:"PROJECT_CLEANUP_INTERVAL" envDefault:"1h"`

	// CRM v5: n8n webhook для нотификаций по project.* событиям. Пусто →
	// диспатч выключен (события успешно обрабатываются как no-op, не
	// зависают в outbox). N8nWebhookToken — опциональный bearer для
	// authentication в n8n; рекомендуется в проде.
	N8nWebhookURL   string `env:"N8N_WEBHOOK_URL"`
	N8nWebhookToken string `env:"N8N_WEBHOOK_TOKEN"`

	// N8nEmailWebhookURL — отдельный workflow в n8n для email.*
	// (verify + password reset). Пусто → email шлёт встроенный mailer
	// (UniSender Go) как раньше; если и mailer пустой — событие пишется
	// в log и квитируется (см. cmd/worker emailHandler).
	N8nEmailWebhookURL string `env:"N8N_EMAIL_WEBHOOK_URL"`

	// N8nSupportWebhookURL — workflow для обращений в поддержку из футера UI
	// (event support.message_received). Пусто → событие квитируется как
	// no-op (запись в support_messages остаётся, но в Telegram не уходит).
	N8nSupportWebhookURL string `env:"N8N_SUPPORT_WEBHOOK_URL"`

	// S3SweepAccessKey / S3SweepSecretKey — отдельный сервис-аккаунт для
	// orphan-sweep'a в worker'е и CLI cmd/s3-sweep-once. Требует list +
	// delete на bucket. Принцип наименьших привилегий: основной S3_ACCESS_KEY
	// (которым подписываются presigned PUT'ы для фронта) должен быть upload-
	// only, без list/delete. Если эти ключи пусты — sweep выключен (no-op),
	// фронт-аплоад продолжает работать.
	S3SweepAccessKey string `env:"S3_SWEEP_ACCESS_KEY"`
	S3SweepSecretKey string `env:"S3_SWEEP_SECRET_KEY"`
	// S3OrphanMinAge — мин. возраст объекта S3 до того, как его можно
	// удалить как orphan. Должен быть заметно больше portfolioUploadExpiry
	// (15m), чтобы не прибить in-flight presigned upload до записи в БД.
	// Дефолт 24h: с большим запасом покрывает любые штатные задержки.
	S3OrphanMinAge time.Duration `env:"S3_ORPHAN_MIN_AGE" envDefault:"24h"`
	// S3SweepInterval — периодичность sweep'a. 6h по умолчанию:
	// orphan'ы не сильно горят (S3-расходы накапливаются медленно), а
	// листинг bucket'а на сотнях тысяч объектов не дёшев. <=0 → выключено.
	S3SweepInterval time.Duration `env:"S3_SWEEP_INTERVAL" envDefault:"6h"`

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
