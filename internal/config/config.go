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
	// OpenSearchReindexOnStart — если true, worker при старте DROP'нет оба
	// индекса и создаст заново с текущим маппингом, потом bootstrap'ом
	// переиндексирует всех published-approved спецов. Используется при
	// смене analyzer'a (например, добавили synonyms) — иначе старый индекс
	// использует старые правила. Даунтайм поиска ~5-30 сек в зависимости
	// от объёма. Ставить в true ночью на один запуск, потом обратно false.
	// См. docs/DEPLOY.md → «Reindex на смене маппинга».
	OpenSearchReindexOnStart bool `env:"OPENSEARCH_REINDEX_ON_START" envDefault:"false"`

	RedisAddr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	RedisPassword string `env:"REDIS_PASSWORD"`
	RedisDB       int    `env:"REDIS_DB" envDefault:"0"`

	// ACCOUNTS_SECRET_KEY — ключ шифрования паролей от аккаунтов бренда
	// (base64, 32 байта). Пусто — пароли не хранятся вовсе, ручки
	// отвечают 501. В базе ключа нет намеренно: иначе шифрование не
	// отличается от хранения текстом.
	AccountsSecretKey string `env:"ACCOUNTS_SECRET_KEY"`

	S3Endpoint  string `env:"S3_ENDPOINT" envDefault:"http://localhost:9000"`
	S3AccessKey string `env:"S3_ACCESS_KEY"`
	S3SecretKey string `env:"S3_SECRET_KEY"`
	S3Bucket    string `env:"S3_BUCKET" envDefault:"marketpclce"`
	S3Region    string `env:"S3_REGION" envDefault:"us-east-1"`
	S3UseSSL    bool   `env:"S3_USE_SSL" envDefault:"false"`
	// Опциональный публичный домен origin'a (bucket CNAME): если задан,
	// origin URL собирается как `${S3_PUBLIC_URL}/${key}`. Иначе —
	// `${S3_ENDPOINT}/${S3_BUCKET}/${key}` (path-style на YC по умолчанию).
	S3PublicURL string `env:"S3_PUBLIC_URL"`
	// CDN_BASE_URL — Yandex Cloud CDN перед bucket'ом. Пусто = CDN
	// выключен, юзеры читают напрямую с origin. Заполнен → s3.Client.
	// PublicURL(key) возвращает CDN URL, отдача идёт через edge-кеш,
	// бэкенд видит только miss'ы. Setup-гайд: docs/CDN_SETUP.md.
	// Формат: https://cdn-xxx.yandexcloud.net ИЛИ свой CNAME без trailing /.
	CDNBaseURL string `env:"CDN_BASE_URL"`

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
	AppBaseURL string `env:"APP_BASE_URL" envDefault:"http://localhost:5173"`

	// Вход через Яндекс. Пусто = кнопка на фронте не показывается и ручка
	// отвечает 501: локальный запуск без OAuth должен работать.
	YandexClientID     string `env:"YANDEX_CLIENT_ID"`
	YandexClientSecret string `env:"YANDEX_CLIENT_SECRET"`
	// Должен совпадать с тем, что зарегистрирован в кабинете Яндекса,
	// иначе обмен кода вернёт invalid_grant.
	YandexRedirectURI string `env:"YANDEX_REDIRECT_URI"`
	// SPAShellURL — откуда API берёт index.html, чтобы подставить в него
	// og-мету конкретного специалиста (см. internal/profiles/og.go).
	// В проде это Caddy внутри docker-сети. Пусто — ручка /specialist/{id}
	// не монтируется, ссылки разворачиваются общей метой сайта.
	SPAShellURL         string        `env:"SPA_SHELL_URL" envDefault:"http://web/index.html"`
	EmailVerifyTokenTTL time.Duration `env:"EMAIL_VERIFY_TOKEN_TTL" envDefault:"24h"`
	RateEmailResendPer  time.Duration `env:"RATE_EMAIL_RESEND_PER" envDefault:"60s"`
	// EmailVerificationDisabled — выключает весь soft-gate целиком: юзер при
	// регистрации сразу email_verified=true, publish/leads проходят без
	// проверки, resend/verify становятся no-op'ами. Для локального запуска
	// без n8n. В проде .env.prod не должен ставить true.
	EmailVerificationDisabled bool `env:"EMAIL_VERIFICATION_DISABLED" envDefault:"false"`

	// Привязка аккаунта к «Боту Работ»: соседний продукт даёт партнёрскую
	// цену тем, кто зарегистрирован у нас. Подтверждаем мы, а не человек
	// словами, — поэтому нужен общий секрет и адрес его вебхука. Пусто —
	// ручка не поднимается вовсе.
	PartnerSecret      string `env:"PARTNER_SECRET"`
	BotrabotWebhookURL string `env:"BOTRABOT_WEBHOOK_URL"`

	// ── Телеграм-боты ──────────────────────────────────────────────
	//
	// Ботов два: креаторский и клиентский. Сам бот живёт отдельным
	// сервисом и ходит в api.telegram.org сам; у нас — источник истины
	// (привязки), проверка initData и отбор получателей.
	//
	// Токены держим и у себя: проверка подписи мини-аппа — чистая
	// криптография над токеном бота, и гонять её через чужой сервис
	// значит уронить вход в мини-апп вместе с ним. Пусто — вход через
	// этого бота отвечает 501, а не пятисоткой.
	TelegramCreatorBotToken    string `env:"TELEGRAM_CREATOR_BOT_TOKEN"`
	TelegramCreatorBotUsername string `env:"TELEGRAM_CREATOR_BOT_USERNAME"`
	TelegramClientBotToken     string `env:"TELEGRAM_CLIENT_BOT_TOKEN"`
	TelegramClientBotUsername  string `env:"TELEGRAM_CLIENT_BOT_USERNAME"`
	// TelegramInitDataTTL — сколько живёт подписанная строка мини-аппа.
	// Сутки: webview держат открытым, и вкладку возвращают к жизни на
	// следующий день — отказывать такому человеку незачем, а вечная
	// подпись становится ключом от аккаунта.
	TelegramInitDataTTL time.Duration `env:"TELEGRAM_INITDATA_TTL" envDefault:"24h"`

	// Переменных BOT_WEBHOOK_* здесь больше нет, и это не упущение.
	//
	// Уведомления ботам мы не шлём: воркер кладёт их в bot_messages, а
	// сервис бота забирает сам (GET /api/v1/bot/messages). Прежняя
	// схема зависела от того, дозвонится ли наша ВДС до чужого
	// облака, — первого октября 2026 маршрут до Railway оборвался
	// внутри сети хостера, и уведомление потерялось. См.
	// internal/telegram/queue.go.
	// BotSharedSecret — общий секрет для входящих /api/v1/bot/*. Пусто
	// — группа ручек не поднимается вовсе: открытая привязка чужого
	// телеграма к чужому аккаунту хуже отсутствующей.
	BotSharedSecret string `env:"BOT_SHARED_SECRET"`

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
	RateAuthPerMin  int `env:"RATE_AUTH_PER_MIN" envDefault:"10"`
	RateAuthPerHour int `env:"RATE_AUTH_PER_HOUR" envDefault:"60"`

	// Выдача presigned-ссылок на загрузку. Отдельный лимит, потому что это
	// единственное место, где залогиненный человек тратит наши деньги: одна
	// ссылка — один объект в бакете, а размер объявляет клиент и подпись его
	// не навязывает. Двадцати в минуту хватает даже фото-кейсу из десяти
	// снимков с перезаливкой, а скрипту — уже нет.
	RateUploadPerMin  int `env:"RATE_UPLOAD_PER_MIN" envDefault:"20"`
	RateUploadPerHour int `env:"RATE_UPLOAD_PER_HOUR" envDefault:"200"`

	// CRM endpoints (/me/projects, /me/specialist, /manager, /admin).
	// Лимит per user+ip — менеджеры/админы пишут плотно, но не должно быть
	// возможности устроить шторм action-endpoint-ов (start/skip/approve в
	// бесконечном цикле, например).
	RateCRMPerMin  int `env:"RATE_CRM_PER_MIN" envDefault:"120"`
	RateCRMPerHour int `env:"RATE_CRM_PER_HOUR" envDefault:"3000"`

	// Обновление просмотров по заходу в карточку — ВТОРОЕ место, где
	// залогиненный человек тратит наши деньги, и дороже первого: один
	// запрос уходит в сторонний сборщик пачкой до двадцати пяти ссылок,
	// каждая из которых стоит кредит.
	//
	// Поэтому свой лимит, а не общий crm: 120 запросов в минуту по
	// двадцать пять ссылок — это три тысячи платных обращений в минуту
	// с одного аккаунта, и у поставщика всего два параллельных слота.
	// Шесть в минуту — это открыть карточку, обновить и переоткрыть
	// несколько раз подряд; скрипту уже нет.
	RateRefreshPerMin  int `env:"RATE_REFRESH_PER_MIN" envDefault:"6"`
	RateRefreshPerHour int `env:"RATE_REFRESH_PER_HOUR" envDefault:"60"`

	// Outbox-воркер. MaxAttempts/BackoffCap определяют поведение ретраев и
	// порог для DLQ (dead_at). Retention/CleanupInterval — TTL на обработанные
	// записи, OutboxDeadRetention — отдельный, намного больший срок на
	// мёртвые: они описывают происшествие и разбираются руками, но без
	// верхней границы таблица растёт бесконечно. WorkerMetricsAddr — отдельный
	// HTTP-listener у воркера для /metrics (alloy скрейпит worker:9090/metrics).
	OutboxMaxAttempts     int           `env:"OUTBOX_MAX_ATTEMPTS" envDefault:"10"`
	OutboxBackoffCap      time.Duration `env:"OUTBOX_BACKOFF_CAP" envDefault:"10m"`
	OutboxRetention       time.Duration `env:"OUTBOX_RETENTION" envDefault:"168h"`
	OutboxDeadRetention   time.Duration `env:"OUTBOX_DEAD_RETENTION" envDefault:"4320h"`
	OutboxCleanupInterval time.Duration `env:"OUTBOX_CLEANUP_INTERVAL" envDefault:"1h"`
	WorkerMetricsAddr     string        `env:"WORKER_METRICS_ADDR" envDefault:":9090"`

	// Транскод-пайплайн preview-видео (см. docs/VIDEO_TRANSCODING.md).
	// FFmpegPath пустой → exec.LookPath("ffmpeg") (берётся первый из PATH).
	// Если ffmpeg не найден на старте воркера — обработчик
	// portfolio.video_uploaded стартует как no-op (логирует и квитирует),
	// чтобы воркер запускался и в dev-окружении без ffmpeg.
	FFmpegPath       string        `env:"FFMPEG_PATH" envDefault:""`
	TranscodeTimeout time.Duration `env:"TRANSCODE_TIMEOUT" envDefault:"90s"`
	TranscodeTempDir string        `env:"TRANSCODE_TEMP_DIR" envDefault:"/tmp/transcode"`

	// CRM v5: review-шаг ставится в waiting_client с deadline=now+ReviewDeadline.
	// По истечении worker переводит шаг в skipped. Дефолт 7 дней.
	ReviewDeadline      time.Duration `env:"REVIEW_DEADLINE" envDefault:"168h"`
	ReviewCheckInterval time.Duration `env:"REVIEW_CHECK_INTERVAL" envDefault:"1h"`

	// PublicationRemindersInterval — как часто воркер проверяет, кому пора
	// напомнить о выкладке. Час: отправка всё равно дедуплицируется по
	// дате, поэтому частый тик не приводит к повторным сообщениям, а
	// только сокращает задержку после рестарта.
	PublicationRemindersInterval time.Duration `env:"PUBLICATION_REMINDERS_INTERVAL" envDefault:"1h"`
	// PublicationRemindersAfterHour — час местного времени, с которого
	// можно писать креаторам. До него проход выполняется вхолостую.
	PublicationRemindersAfterHour int `env:"PUBLICATION_REMINDERS_AFTER_HOUR" envDefault:"9"`

	// Сбор статистики по сданным роликам (instacurl). Без URL и ключа
	// сбор не запускается вовсе: пустой отчёт читается как «ролики никто
	// не смотрит», и это худшая из подмен.
	InstacurlURL     string        `env:"INSTACURL_URL"`
	InstacurlAPIKey  string        `env:"INSTACURL_API_KEY"`
	InstacurlTimeout time.Duration `env:"INSTACURL_TIMEOUT" envDefault:"60s"`
	// StatsCollectInterval — как часто воркер проверяет, кого пора
	// обойти. Частый тик безопасен: частоту обхода задаёт расписание
	// ссылки (collect_every), а не период тикера, и когда брать нечего,
	// тик стоит один запрос по индексу.
	//
	// Минута, а не час. Фоновый шаг суточный (collectEvery), и минутный
	// тик его не ускоряет — он нужен для другого: только что сданная
	// ссылка приходит с next_collect_at = now(), и при часовом тике её
	// первый замер ждал бы до часа. Это и была жалоба «выложил, а в
	// кабинете ноль».
	//
	// Обновление по заходу в карточку живёт не здесь: там цифры
	// подтягиваются целиком и каждый раз, без расписания свежести.
	StatsCollectInterval time.Duration `env:"STATS_COLLECT_INTERVAL" envDefault:"1m"`
	// StatsCollectBatch — сколько ссылок за ОДИН поход в instacurl.
	//
	// Десять, а не сорок: обход одной ссылки VK занимает ~3,5 секунды
	// (замер на живом клипе) и они идут последовательно — внутри
	// instacurl Chromium запускается по одному. Сорок ссылок, из которых
	// восемь-десять на VK, не укладывались в таймаут клиента в 60 секунд,
	// и такая пачка не собиралась никогда.
	StatsCollectBatch int `env:"STATS_COLLECT_BATCH" envDefault:"10"`
	// StatsCollectBatchesPerTick — сколько пачек прогонять за один тик.
	//
	// Нужен именно потому, что пачка стала мелкой: при часовом тике
	// десять ссылок за проход — это 240 в сутки, чего не хватит уже на
	// одного креатора с шестьюдесятью роликами на пяти площадках.
	// Прогоняем пачки подряд, пока есть что собирать, но не больше этого
	// числа — чтобы один тик не работал бесконечно.
	StatsCollectBatchesPerTick int `env:"STATS_COLLECT_BATCHES_PER_TICK" envDefault:"20"`

	// Обход аккаунтов креаторов: сервис сам находит новые ролики на
	// площадках и кладёт их в «это ваш ролик?».
	//
	// ВЫКЛЮЧЕН ПО УМОЛЧАНИЮ, и это не осторожность ради осторожности.
	// Обход стоит кредит на аккаунт в сутки НЕЗАВИСИМО от того, вышло
	// там что-нибудь или нет, и в отличие от сбора по ссылкам он не
	// затихает со временем: проект с тремя креаторами на пяти площадках
	// — это пятнадцать обходов каждый день, пока проект жив. Включать
	// его должен тот, кто посмотрел на остаток кредитов (см.
	// docs/MONITORING.md, «Сколько сбор статистики стоит в обходах»).
	AccountScanEnabled bool `env:"ACCOUNT_SCAN_ENABLED" envDefault:"false"`
	// AccountScanInterval — как часто воркер проверяет, кого пора
	// обойти.
	//
	// Час, а не шесть. Частый тик безопасен и раньше: правило «аккаунт
	// не чаще раза в сутки» держит выборка, а не тикер, и пустой тик
	// стоит один запрос по индексу. Но с шестью часами он стал ещё и
	// неверным.
	//
	// Очередь обхода открывается В ПОЛНОЧЬ целиком (MarkScanned ставит
	// next_scan_at на следующую полночь — решение владельца от 2
	// октября). За тик берётся batch × batchesPerTick = 25 аккаунтов,
	// значит при шестичасовом тике двадцать шестой аккаунт ждёт до
	// шести утра, пятьдесят первый — до полудня. «Раз в сутки в 00:00»
	// превращается в «по четвертинке в день», и для срезов аудитории это
	// не оттенок: прирост считается по границам периода, то есть по
	// датам.
	//
	// С часом очередь разгребается за первые часы после полуночи, а
	// расход не растёт: суточное правило всё равно не даст сходить по
	// аккаунту дважды. Дороже становятся только пустые тики — по одному
	// запросу каждый.
	AccountScanInterval time.Duration `env:"ACCOUNT_SCAN_INTERVAL" envDefault:"1h"`
	// AccountScanBatch — сколько аккаунтов за ОДИН поход в instacurl.
	//
	// Пять, а не десять, как у ссылок: разбор профиля тяжелее разбора
	// одного ролика (у VK это страница со списком постов через
	// Chromium), а таймаут клиента общий — шестьдесят секунд. Пачка, не
	// уложившаяся в таймаут, не собирается никогда.
	AccountScanBatch int `env:"ACCOUNT_SCAN_BATCH" envDefault:"5"`
	// AccountScanBatchesPerTick — сколько пачек за тик.
	//
	// 5 × 5 = 25 аккаунтов за тик. Потолок суток задаёт не это число, а
	// правило «аккаунт не чаще раза в календарный день»: сколько бы
	// тиков ни прошло, второй раз за аккаунт мы не заплатим. Тики лишь
	// определяют, за сколько часов после полуночи разгребётся очередь —
	// при часовом тике это 25 аккаунтов в час.
	AccountScanBatchesPerTick int `env:"ACCOUNT_SCAN_BATCHES_PER_TICK" envDefault:"5"`

	// BillingPeriodLockDelay — через сколько после конца периода он
	// подытоживается сам. Четырнадцать дней — правило площадки: за две
	// недели ролик набирает основную массу просмотров, дальше счёт почти
	// не меняется. До появления этого срока правило нигде не
	// исполнялось — всё держалось на том, что менеджер вовремя нажал
	// «Пересчитать».
	BillingPeriodLockDelay time.Duration `env:"BILLING_PERIOD_LOCK_DELAY" envDefault:"336h"`
	// BillingPeriodLockInterval — как часто искать периоды, которым пора
	// закрыться. Час: опоздание на час после двухнедельной отсрочки
	// никого не трогает, а проход по пустой выборке ничего не стоит.
	BillingPeriodLockInterval time.Duration `env:"BILLING_PERIOD_LOCK_INTERVAL" envDefault:"1h"`

	// OrderExpiryInterval — как часто проверять протухшие приглашения.
	// Приглашение живёт трое суток, но место надо отдавать следующему
	// быстро: час — компромисс между «клиент ждёт зря» и лишними
	// проходами по пустой выборке.
	OrderExpiryInterval time.Duration `env:"ORDER_EXPIRY_INTERVAL" envDefault:"1h"`

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
