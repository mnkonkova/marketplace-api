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
cmd/api              точка входа
internal/
  config             загрузка env
  httpapi            chi router, middleware, handlers
  platform/{db,es,s3,redisx}  инфраструктурные клиенты
  auth               регистрация/логин/JWT
  profiles           профили специалистов и портфолио
  catalog            справочники категорий и навыков
  search             обёртка над OpenSearch + индексация
  summarize          LLM-подбор по результатам поиска
  leads              заявки клиентов
  media              загрузка/обработка медиа
  outbox             outbox-паттерн для индексации
migrations           goose SQL миграции
```

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
| GET    | `/categories`  | Список категорий специалистов                              |
| GET    | `/skills`      | Список навыков (filter `kind=tool|platform|genre`)         |

### search & feed (read, под общим rate-limit `read`)

| Method | Path                          | Описание                                                |
| ------ | ----------------------------- | ------------------------------------------------------- |
| GET    | `/search`                     | Поиск спецов: `q`, `category`, `skill`, `city`, `rate_min`, `rate_max`, `limit`, `offset` |
| GET    | `/specialists`                | Алиас `/search`                                         |
| GET    | `/specialists/{id}`           | Публичный профиль (включает первые 20 отзывов)          |
| GET    | `/specialists/{id}/reviews`   | Пагинированный листинг отзывов: `limit`, `offset`       |
| GET    | `/categories/stats`           | Счётчики опубликованных спецов по категориям            |
| GET    | `/feed`                       | Лента портфолио по категориям, cursor-пагинация         |

### LLM (доступны только при `LLM_API_KEY != ""`)

| Method | Path                  | Описание                                                  |
| ------ | --------------------- | --------------------------------------------------------- |
| POST   | `/search/summarize`   | LLM-подбор «топ-N» по результатам поиска (Redis-кеш + RL) |
| POST   | `/clarify`            | Уточняющий диалог: следующий вопрос или финальный запрос  |

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
