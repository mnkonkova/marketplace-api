# marketpclce

Discovery-маркетплейс специалистов (монтаж/СММ/UGC/блогеры/таргет/посевы). MVP без платежей.

## Стек

Go 1.22 (chi, pgx, goose) · PostgreSQL 16 · OpenSearch 2 · Redis · MinIO (S3) · LLM (anthropic/deepseek). Фронт — статика в `web/` (index.html, app.js, styles.css).

## Команды

`make up` (compose), `make migrate-up`, `make run` (API :8080), `make run-worker` (outbox-индексер), `make seed`, `make test`, `make lint`, `make fmt`. Goose-миграции: `make migrate-create name=xxx`. Конфиг — env (см. `.env.example`); `JWT_SECRET` и `DATABASE_URL` обязательны.

## Структура

- `cmd/{api,worker,seed}` — точки входа.
- `internal/config` — `env.Parse` в `Config`.
- `internal/httpapi` — chi-router (`router.go`), `RateLimit` middleware, `handlers/{health,stub}`.
- `internal/platform/{db,es,redisx,s3}` — тонкие клиенты. ES — самописный (без офиц. SDK), `EnsureIndex` ставит маппинг.
- Домены (`auth`, `catalog`, `profiles`, `search`, `summarize`, `clarify`, `profilecheck`, `leads`, `media`) — паттерн `repo.go` (pgx) → `service.go` → `handlers.go` (+ `dto.go` где нужно).
- `internal/outbox` — outbox-паттерн: домены пишут события в одной транзакции, `cmd/worker` забирает и зовёт `search.indexer` (см. `internal/search/indexer.go`).
- `internal/llm` — `Provider` интерфейс, реализации `anthropic.go` / `deepseek.go`.
- `internal/ratelimit` — Redis-лимитер, окна задаются в роутере (`read`/`leads`/`summarize`).
- `migrations/*.sql` — goose. Основные таблицы: `users`, `specialist_profiles`, `specialty_categories`, `specialist_categories` (uniq primary), `skills`, `specialist_skills`, `portfolio_items`, `outbox`, `reviews`.

## Поиск (`internal/search/service.go`)

Hard-фильтры: `is_published`, `skill_slugs`. Soft: `city`, `rate_min/max` (попадают в bool.filter, но релаксируются). Категории — в `post_filter` (чтобы агрегация по категориям не схлопывалась). При `Total < 5` и наличии soft-фильтров идёт второй запрос без них → возвращается в `similar` + `relaxed`. Старый бродинг: при пустом текстовом результате повторный запрос с `Q=""` (`broadened: true`).

## Соглашения

- Источник истины для поиска — OpenSearch (через outbox), не Postgres-чтения напрямую.
- LLM-эндпоинты (`/search/summarize`, `/clarify`, `/me/profile/check`) опциональны — без `LLM_API_KEY` хендлеры не маунтятся / возвращают пусто.
- Без Redis API стартует, но rate-limit и кеш summarize отключены (логируется warn).

## Не трогать без причины

`migrations/00001_init.sql` (initial schema), `internal/search/mapping.go` (поменяешь — переиндексировать).
