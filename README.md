# marketpclce

Discovery-маркетплейс специалистов: монтаж, видеорежиссура, моушн, СММ, UGC, блогеры, таргет+SEO, посевы.
MVP: каталог + поиск (OpenSearch) + LLM-подытоживание + заявки. Без платежей.

## Стек

- Go 1.22, chi, pgx, goose
- PostgreSQL 16
- OpenSearch 2 (поиск)
- Yandex Object Storage (S3) — медиа портфолио
- Redis (кеш, rate-limit)

## Локальный запуск

Требования: Go 1.22+, Docker, Docker Compose v2.

```bash
cp .env.example .env
make up            # postgres, opensearch, redis
make tidy          # go mod tidy
make migrate-up    # накатить миграции
make run           # запустить API на :8080
make run-worker    # в другом терминале — outbox-индексер
```

S3 (Yandex Object Storage) подключается опционально: если в `.env`
не заданы `S3_ACCESS_KEY`/`S3_SECRET_KEY` — API стартует, но
загрузка видео в портфолио (`/me/portfolio/upload-url`) вернёт 503.

Проверка: `curl localhost:8080/healthz`.

## Деплой и redeploy

Прод-стек поднимается на одной VDS через `docker-compose.prod.yml`: postgres,
opensearch, redis, api, worker, web (Caddy + статика фронта). Полная
шпаргалка по первому развёртыванию — в [`docs/DEPLOY.md`](docs/DEPLOY.md):
DNS, ufw, sysctl, заполнение `.env.prod`.

Раскладка на сервере (оба репо рядом):
```
/opt/marketpclce/
├── api/   ← этот репо, отсюда запускаются make-цели
└── web/   ← git clone marketplace-web (compose билдит фронт из ../web)
```

### Первый запуск

```bash
cd /opt/marketpclce/api
cp .env.prod.example .env.prod && nano .env.prod   # секреты — см. docs/DEPLOY.md
make deploy                                         # build + migrate + start всего стека
```

### Повседневные обновления — `make redeploy`

Zero-downtime передеплой: образы билдятся **заранее**, пока старые
контейнеры обслуживают трафик. Потом graceful shutdown (Go API ловит
SIGTERM, дослуживает активные запросы), recreate, Caddy ретраит `/api/*`
на время рестарта — пользователь видит «медленный» запрос, не 502.

```bash
make redeploy        # git pull api+web → build → goose up → recreate api/worker/web
make redeploy-api    # только Go (api + worker)
make redeploy-web    # только фронт (без миграций и без рестарта Go)
```

Флаги через ENV:
```bash
SKIP_PULL=1    make redeploy   # без git pull (правки уже на сервере)
SKIP_MIGRATE=1 make redeploy   # без goose up (только код)
```

Под капотом — [`scripts/redeploy.sh`](scripts/redeploy.sh). Идемпотентен:
`goose up` пропустит уже применённые миграции, `docker compose build`
переиспользует слои.

### Когда `redeploy` **не** zero-downtime

- **Breaking migration** (`DROP COLUMN`, `RENAME`, не-аддитивные изменения):
  старый api сломается на новой схеме. Используй expand-contract: сначала
  аддитивная миграция + код, читающий обе формы → потом drop. Или прими
  короткий downtime через `make deploy`.
- **Breaking API contract** (фронт ждёт новое поле, бэк ещё старый):
  деплой по порядку — сначала `make redeploy-api`, потом `make redeploy-web`.

### Мониторинг

Метрики (HTTP/Go runtime/CPU/RAM) и логи всех контейнеров пушатся в
**Grafana Cloud free tier** через Grafana Alloy. Self-hosted Prometheus
поднимать не нужно. Setup и список алертов — в [`docs/MONITORING.md`](docs/MONITORING.md).

`/metrics` эндпоинт API доступен только внутри docker network (Caddy его
не проксирует наружу) — alloy ходит к `api:8080/metrics` напрямую.

### Полезные команды

```bash
make prod-ps                                  # статусы всех сервисов
make prod-logs                                # tail логов
$(PROD_DC) logs -f api worker                 # только Go
$(PROD_DC) logs -f web                        # только Caddy
$(PROD_DC) exec postgres psql -U marketpclce  # SQL-консоль
make prod-seed                                # демо-данные
```

(`PROD_DC = docker compose -f docker-compose.prod.yml --env-file .env.prod`)

## Структура

```
cmd/{api,worker,seed,seed-videos}   точки входа
internal/
  config             загрузка env
  httpapi            chi router, middleware, handlers
  httpx              общий WriteJSON/WriteErr для всех хендлеров
  platform/{db,es,s3,redisx}  инфраструктурные клиенты
  auth               регистрация/логин/JWT
  profiles           профили специалистов и портфолио
  catalog            справочники категорий (с типом Производство/Продвижение) и навыков
  search             обёртка над OpenSearch: индекс `specialists` + индекс `feed_videos` (один doc = одно видео + денорм спеца)
  feed               лента видео: ES search_after, diversity round-robin, Redis-кэш
  llm                Provider-интерфейс + реализации anthropic / deepseek
  summarize          LLM-подбор по результатам поиска
  clarify            LLM-диалог
  profilecheck       LLM-валидация bio/имени при публикации
  leads              заявки клиентов
  reviews            отзывы
  outbox             outbox-паттерн (specialist.upserted → reindex обоих индексов)
  ratelimit          Redis-окна для read/leads/summarize/clarify
migrations           goose SQL миграции (00001 — initial с актуальной схемой)
```

### Concurrency / целостность

- `PATCH /me/profile`, `PUT /me/profile/{categories,skills}`, `PUT /me/portfolio/{id}/categories`,
  `PATCH /reviews/{id}`, `PATCH /me/leads/{id}/recipient` — optimistic locking
  через `updated_at` (необязательное поле в body). Несовпадение → **409
  `stale_updated_at`**, фронт должен перечитать и применить заново. Без поля
  — старое поведение (для обратной совместимости).
- Outbox-worker использует `SELECT FOR UPDATE SKIP LOCKED` — безопасно
  масштабируется по репликам.
- `reviews` имеет PG-триггер `reviews_recalc_trg`, пересчитывающий
  `rating_avg`/`reviews_count` под row-lock'ом.

## Миграции

```bash
make migrate-up
make migrate-status
make migrate-create name=add_reviews
```

## API

Префикс всех ручек ниже — `/api/v1`. JSON-формат, ошибки — `{ "error": "..." }`.
Аутентификация — `Authorization: Bearer <access_token>`.

Интерактивная документация: `http://localhost:8080/swagger/index.html`
(spec: `/swagger/doc.json`). Перегенерация — `make swag` после изменений.

### auth

| Method | Path                | Auth | Описание                                                  |
| ------ | ------------------- | ---- | --------------------------------------------------------- |
| POST   | `/auth/register`    | —    | Регистрация: `email`/`phone` + `password`, `kind`         |
| POST   | `/auth/login`       | —    | Логин по логину (email/phone) и паролю → пара токенов     |
| POST   | `/auth/refresh`     | —    | Обмен `refresh_token` → новая пара токенов                |
| GET    | `/me`               | ✅   | Профиль пользователя из JWT                               |

### catalog

| Method | Path           | Описание                                                   |
| ------ | -------------- | ---------------------------------------------------------- |
| GET    | `/categories`  | Список категорий специалистов. Поле `type` = `Производство` / `Продвижение` |
| GET    | `/skills`      | Список навыков (filter `kind=tool|platform|genre`)         |

### search & feed (read, под общим rate-limit `read`)

| Method | Path                          | Описание                                                |
| ------ | ----------------------------- | ------------------------------------------------------- |
| GET    | `/search`                     | Поиск спецов: `q`, `category`, `skill`, `city`, `rate_min`, `rate_max`, `limit`, `offset` |
| GET    | `/specialists/{id}`           | Публичный профиль (включает первые 20 отзывов)          |
| GET    | `/specialists/{id}/reviews`   | Пагинированный листинг отзывов: `limit`, `offset`       |
| GET    | `/categories/stats`           | Счётчики опубликованных спецов по категориям            |
| GET    | `/feed`                       | Лента видео: `q`, `category` (csv), `skill` (csv), `city`, `per_specialist`, `cursor`, **`ids` (csv user_id, до 100 — жёсткий фильтр после `/search` или `/search/summarize`)**. Cursor-пагинация. См. ниже **Архитектуру feed**. |

### LLM (доступны только при `LLM_API_KEY != ""`)

| Method | Path                  | Rate-limit                          | Описание                                                  |
| ------ | --------------------- | ----------------------------------- | --------------------------------------------------------- |
| POST   | `/search/summarize`   | `summarize` (5/min, 30/hour) после cache-miss | LLM-подбор «топ-N» по результатам поиска (Redis-кеш + RL) |
| POST   | `/clarify`            | `clarify` (15/min, 120/hour)         | Уточняющий диалог: следующий вопрос или финальный запрос. Stateless на сервере — клиент шлёт всю историю каждым запросом. |

### leads (rate-limit `leads`)

| Method | Path                              | Auth | Описание                                                    |
| ------ | --------------------------------- | ---- | ----------------------------------------------------------- |
| POST   | `/leads`                          | opt  | Создать заявку, отправить выбранным спецам. Ответ — id + контакты |
| GET    | `/me/leads/incoming`              | ✅   | Входящие заявки специалиста (`status` filter, `limit/offset`) |
| PATCH  | `/me/leads/{id}/recipient`        | ✅   | Сменить статус получателя: `viewed`/`accepted`/`declined`    |

### profile (мой профиль и портфолио, all auth)

| Method | Path                                  | Описание                                       |
| ------ | ------------------------------------- | ---------------------------------------------- |
| GET    | `/me/profile`                         | Свой профиль с контактами                      |
| PATCH  | `/me/profile`                         | Частичный апдейт полей                         |
| PUT    | `/me/profile/categories`              | Заменить категории + primary                   |
| PUT    | `/me/profile/skills`                  | Заменить список навыков                        |
| POST   | `/me/profile/publish`                 | Опубликовать (есть LLM-валидация bio/имени)    |
| POST   | `/me/profile/unpublish`               | Снять с публикации                             |
| POST   | `/me/profile/check`                   | LLM-проверка bio/имени без публикации          |
| GET    | `/me/portfolio`                       | Свои элементы портфолио                        |
| POST   | `/me/portfolio`                       | Добавить видео (URL-форма)                     |
| POST   | `/me/portfolio/upload-url`            | Presigned PUT для видео в S3 (требует S3)      |
| POST   | `/me/uploads/image`                   | Presigned PUT для аватара/превью (требует S3)  |
| PUT    | `/me/portfolio/{id}/categories`       | Заменить категории элемента портфолио          |
| DELETE | `/me/portfolio/{id}`                  | Удалить элемент                                |

### reviews (rate-limit `leads`)

| Method | Path                        | Auth | Описание                                                |
| ------ | --------------------------- | ---- | ------------------------------------------------------- |
| POST   | `/reviews`                  | ✅   | Создать отзыв. При `lead_id != null` проверяет, что клиент == автор лида и target — accepted-получатель. Триггер пересчитывает `rating_avg`/`reviews_count`, outbox реиндексит OpenSearch |
| PATCH  | `/reviews/{id}`             | ✅   | Изменить `rating`/`text` (только автор)                 |
| DELETE | `/reviews/{id}`             | ✅   | Удалить отзыв (только автор)                            |

### health

| Method | Path        | Описание                                |
| ------ | ----------- | --------------------------------------- |
| GET    | `/healthz`  | Liveness                                |
| GET    | `/readyz`   | Readiness (включая PG)                  |

## Архитектура `/feed`

Лента построена поверх **отдельного OpenSearch-индекса `feed_videos`**: один документ = одно видео + денормализованные поля специалиста (`display_name`, `avatar_url`, `rating_avg`, `reviews_count`, `city`, etc.). Не путать с индексом `specialists` (один документ = один спец) — он остался для `/search`/`/categories/stats`.

### Индексация

- Источник истины — PostgreSQL (`portfolio_items` + `specialist_profiles`).
- Worker слушает outbox-события `specialist.upserted` / `specialist.deleted`.
- На `upserted`: `search.Indexer.Reconcile` (специалист в `specialists`) **и** `search.FeedIndexer.ReconcileVideos` (все его видео в `feed_videos`).
- `ReconcileVideos` = `delete_by_query user_id=X` + upsert каждого актуального видео. Идемпотентно.
- Outbox-событие пишется в одной транзакции с любой записью в `portfolio_items` или `specialist_profiles` (см. `internal/profiles/service.go`).
- При первом запуске воркера на новом инстансе (`feed_videos` пуст) автоматически прогоняется bootstrap по всем опубликованным спецам.

### Запрос

```
GET /api/v1/feed?category=editor&q=анимация&cursor=<base64>
```

ES-запрос против `feed_videos`:

- `filter`: `is_published=true`, плюс `terms.category_codes` / `term.city` / `terms.user_id` (для `?ids=...`-флоу).
- `must`: `multi_match` по `display_name`/`title`/`bio`/`description`/`city.text` (если `q` непустой), иначе `match_all`.
- `must_not`: `terms.video_id` (seen-set из курсора).
- `sort`: `rating_avg DESC, video_created_at DESC, video_id ASC` (последний — уникальный tiebreaker, нужен для детерминированного `search_after`).
- `search_after`: sort-key последнего хита прошлой страницы.
- `size`: 50.

### Курсор

Opaque base64-JSON, фронт не парсит. Внутри:
- `sa` — sort-key последнего хита (для ES `search_after`).
- `sv` — seen video_id'шки (FIFO, cap **500**). Защита от дублей при пере-ранжировании индекса между страницами. ~22 KB в URL.

Фронт-флоу: ответ `{items, next_cursor, total}` → следующий запрос с `?cursor=<next_cursor>` повторяя исходные фильтры. Пустой `next_cursor` = лента закончилась.

### Diversity (round-robin внутри страницы)

ES возвращает top-50 по score. `interleaveByUser` группирует их по `user_id` сохраняя ES-порядок внутри группы и раскладывает round-robin: первое видео каждого юзера → второе видео каждого → и т.д. Так 5 видео одного спеца подряд не валятся.

`video_idx` / `video_total` в ответе = позиция и количество видео этого спеца **в текущей странице** (для overlay «N/M»).

### Кэш

Redis, ключ = `feed:cache:sha256(filters+cursor)`, TTL `FEED_CACHE_TTL` (по умолчанию **30s**). Кэшируется для всех — `/feed` публичный, без персонализации. При появлении per-user ленты — добавится skip-cache для авторизованных.

### Поток данных

```
Юзер ищет ─→ /search или /clarify+/summarize
                         │
              user_ids:  ▼
                  /feed?ids=u1,u2,u3
                         │
            (опц.) кэш ─►├─ есть? — отдаём
                         │
                         ▼
              ES feed_videos с terms.user_id
                         │
                         ▼
          interleave by user, build Items, cursor
```

Без `ids`: тот же flow, но с фильтрами по `q`/`category`/`city`.
