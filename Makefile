.PHONY: up down logs ps run build tidy migrate-up migrate-down migrate-status migrate-create test lint fmt swag \
        deploy redeploy redeploy-api redeploy-web prod-up prod-down prod-logs prod-ps prod-build prod-migrate prod-seed prod-seed-videos \
        backup-db prod-backup-db prod-restore-db

DC ?= docker compose
DSN ?= $$(grep -E '^DATABASE_URL=' .env 2>/dev/null | cut -d= -f2- | tr -d '"')
PROD_DSN ?= $$(grep -E '^DATABASE_URL=' .env.prod 2>/dev/null | cut -d= -f2- | tr -d '"')
PROD_DC ?= docker compose -f docker-compose.prod.yml --env-file .env.prod

up:
	$(DC) up -d

down:
	$(DC) down

logs:
	$(DC) logs -f

ps:
	$(DC) ps

tidy:
	go mod tidy

build:
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker
	go build -o bin/seed ./cmd/seed

run:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

seed:
	go run ./cmd/seed

# goose тянет драйверы под все БД по умолчанию (ClickHouse/MySQL/MSSQL/SQLite/…).
# Нам нужен только postgres — исключаем остальные через build tags, чтобы не
# раздувать Docker-образ и module cache.
GOOSE_TAGS := no_clickhouse no_libsql no_mssql no_mysql no_sqlite3 no_vertica no_ydb

migrate-up:
	go run -tags='$(GOOSE_TAGS)' github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DSN)" up

migrate-down:
	go run -tags='$(GOOSE_TAGS)' github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DSN)" down

migrate-status:
	go run -tags='$(GOOSE_TAGS)' github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DSN)" status

migrate-create:
	@test -n "$(name)" || (echo "Usage: make migrate-create name=add_xxx"; exit 1)
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations -s create $(name) sql

test:
	go test ./...

fmt:
	gofmt -w .

lint:
	go vet ./...

# Перегенерировать docs/swagger из аннотаций над хендлерами. После любых
# правок в публичном API (новые ручки, изменения DTO) — `make swag` и
# закоммитить полученные docs/swagger/* в git.
swag:
	go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/api/main.go -o docs/swagger --parseDependency --parseInternal

# ── Prod-стек на VDS (см. docs/DEPLOY.md) ───────────────────────────
deploy:
	./scripts/deploy.sh

# redeploy — то же что deploy, но zero-downtime: образы билдятся ЗАРАНЕЕ,
# контейнеры пересоздаются с graceful shutdown, Caddy ретраит /api/*.
# Параметры через ENV: SKIP_PULL=1 (без git pull), SKIP_MIGRATE=1 (без goose).
redeploy:
	./scripts/redeploy.sh

redeploy-api:
	./scripts/redeploy.sh api

redeploy-web:
	./scripts/redeploy.sh web

prod-up:
	$(PROD_DC) up -d

prod-down:
	$(PROD_DC) down

prod-logs:
	$(PROD_DC) logs -f --tail=200

prod-ps:
	$(PROD_DC) ps

prod-build:
	$(PROD_DC) build api web

prod-migrate:
	$(PROD_DC) run --rm api goose -dir /app/migrations postgres "$(PROD_DSN)" up

prod-seed:
	$(PROD_DC) run --rm api seed

# Заливает 5 сэмплов (Pexels mp4) в S3-бакет под ключами seed/vert-01.mp4…
# seed/vert-05.mp4. Идемпотентно: для каждого слота HEAD; если объект уже
# есть — пропускает. Требует заполненных S3_ACCESS_KEY / S3_SECRET_KEY /
# S3_BUCKET в .env.prod. Нужно один раз — после этого `make prod-seed`
# напишет в БД ссылки на эти объекты, и фид заработает.
prod-seed-videos:
	$(PROD_DC) run --rm api seed-videos

# Бекап БД с ротацией: pg_dump из работающего postgres-контейнера →
# gzip → backups/marketpclce-YYYY-MM-DD_HHMMSS.sql.gz. Удаляет файлы
# старше 3 дней (BACKUP_KEEP_DAYS=3 — переопределяется при вызове).
# Использует exec, а не run --rm — postgres всегда живой в prod'е.
# --clean --if-exists даёт «безопасный restore»: DROP TABLE IF EXISTS
# перед CREATE, идемпотентно поверх существующей БД.
#
# Для регулярного бекапа в cron:
#   0 3 * * * cd /opt/marketplace-api && make prod-backup-db >> backups/cron.log 2>&1
BACKUP_DIR ?= backups
BACKUP_KEEP_DAYS ?= 3
prod-backup-db:
	@mkdir -p $(BACKUP_DIR)
	@PG_USER="$$(grep -E '^POSTGRES_USER=' .env.prod 2>/dev/null | cut -d= -f2- | tr -d '\"')"; \
	 PG_USER="$${PG_USER:-marketpclce}"; \
	 PG_DB="$$(grep -E '^POSTGRES_DB=' .env.prod 2>/dev/null | cut -d= -f2- | tr -d '\"')"; \
	 PG_DB="$${PG_DB:-marketpclce}"; \
	 OUT="$(BACKUP_DIR)/$${PG_DB}-$$(date +%Y-%m-%d_%H%M%S).sql.gz"; \
	 echo "[backup] $$OUT"; \
	 $(PROD_DC) exec -T postgres pg_dump -U "$$PG_USER" -d "$$PG_DB" --no-owner --clean --if-exists | gzip > "$$OUT"; \
	 SIZE=$$(stat -c%s "$$OUT" 2>/dev/null || stat -f%z "$$OUT"); \
	 if [ "$$SIZE" -lt 1024 ]; then \
	   echo "[backup] FAIL: $$OUT < 1KB (pg_dump probably failed)"; \
	   rm -f "$$OUT"; exit 1; \
	 fi; \
	 echo "[backup] OK: $$(numfmt --to=iec $$SIZE 2>/dev/null || echo $$SIZE bytes)"; \
	 find $(BACKUP_DIR) -name "$${PG_DB}-*.sql.gz" -type f -mtime +$(BACKUP_KEEP_DAYS) -print -delete

# Восстановление из последнего бекапа (или указанного через BACKUP_FILE).
# ВНИМАНИЕ: DROP + CREATE, текущие данные затрутся. Только для recovery.
#   make prod-restore-db                              # последний из backups/
#   make prod-restore-db BACKUP_FILE=backups/x.sql.gz # конкретный файл
prod-restore-db:
	@PG_USER="$$(grep -E '^POSTGRES_USER=' .env.prod 2>/dev/null | cut -d= -f2- | tr -d '\"')"; \
	 PG_USER="$${PG_USER:-marketpclce}"; \
	 PG_DB="$$(grep -E '^POSTGRES_DB=' .env.prod 2>/dev/null | cut -d= -f2- | tr -d '\"')"; \
	 PG_DB="$${PG_DB:-marketpclce}"; \
	 FILE="$${BACKUP_FILE:-$$(ls -t $(BACKUP_DIR)/$${PG_DB}-*.sql.gz 2>/dev/null | head -1)}"; \
	 if [ -z "$$FILE" ] || [ ! -f "$$FILE" ]; then \
	   echo "no backup file found in $(BACKUP_DIR)/, set BACKUP_FILE=..."; exit 1; \
	 fi; \
	 echo "[restore] $$FILE → $$PG_DB"; \
	 gunzip -c "$$FILE" | $(PROD_DC) exec -T postgres psql -U "$$PG_USER" -d "$$PG_DB"

# Dev-вариант: тот же бекап, но против локального docker compose
# (DC, .env вместо PROD_DC, .env.prod). Нужен для теста make-логики
# и для разработки — иногда удобно snapshot'нуть локальную БД перед
# рискованной миграцией. Ротация та же.
backup-db:
	@mkdir -p $(BACKUP_DIR)
	@PG_USER="$$(grep -E '^POSTGRES_USER=' .env 2>/dev/null | cut -d= -f2- | tr -d '\"')"; \
	 PG_USER="$${PG_USER:-marketpclce}"; \
	 PG_DB="$$(grep -E '^POSTGRES_DB=' .env 2>/dev/null | cut -d= -f2- | tr -d '\"')"; \
	 PG_DB="$${PG_DB:-marketpclce}"; \
	 OUT="$(BACKUP_DIR)/$${PG_DB}-$$(date +%Y-%m-%d_%H%M%S).sql.gz"; \
	 echo "[backup-dev] $$OUT"; \
	 $(DC) exec -T postgres pg_dump -U "$$PG_USER" -d "$$PG_DB" --no-owner --clean --if-exists | gzip > "$$OUT"; \
	 SIZE=$$(stat -c%s "$$OUT" 2>/dev/null || stat -f%z "$$OUT"); \
	 if [ "$$SIZE" -lt 1024 ]; then \
	   echo "[backup-dev] FAIL: $$OUT < 1KB"; rm -f "$$OUT"; exit 1; \
	 fi; \
	 echo "[backup-dev] OK: $$(numfmt --to=iec $$SIZE 2>/dev/null || echo $$SIZE bytes)"; \
	 find $(BACKUP_DIR) -name "$${PG_DB}-*.sql.gz" -type f -mtime +$(BACKUP_KEEP_DAYS) -print -delete
