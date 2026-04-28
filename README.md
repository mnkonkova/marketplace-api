# marketpclce

Discovery-маркетплейс специалистов: монтаж, видеорежиссура, моушн, СММ, UGC, блогеры, таргет+SEO, посевы.
MVP: каталог + поиск (OpenSearch) + LLM-подытоживание + заявки. Без платежей.

## Стек

- Go 1.22, chi, pgx, goose
- PostgreSQL 16
- OpenSearch 2 (поиск)
- MinIO (S3-совместимое хранилище медиа)
- Redis (кеш, rate-limit)

## Локальный запуск

Требования: Go 1.22+, Docker, Docker Compose v2.

```bash
cp .env.example .env
make up            # postgres, opensearch, redis, minio
make tidy          # go mod tidy
make migrate-up    # накатить миграции
make run           # запустить API на :8080
make run-worker    # в другом терминале — outbox-индексер
```

Проверка: `curl localhost:8080/healthz`.

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
