.PHONY: up down logs ps run build tidy migrate-up migrate-down migrate-status migrate-create test test-db-up test-db-reset test-integration lint fmt swag \
        deploy redeploy redeploy-api redeploy-web prod-up prod-down prod-logs prod-ps prod-build prod-migrate prod-seed prod-seed-videos \
        backup-db prod-backup-db prod-restore-db backup-n8n restore-n8n n8n-import n8n-export \
        grafana-dashboards-push grafana-rules-push

DC ?= docker compose
DSN ?= $$(grep -E '^DATABASE_URL=' .env 2>/dev/null | cut -d= -f2- | tr -d '"')

# Отдельная БД для integration-тестов (тот же postgres-контейнер, другая БД).
# Парсим user/host/port из DATABASE_URL чтобы не дублировать config.
TEST_DB_NAME ?= marketpclce_test
PG_USER ?= $$(echo "$(DSN)" | sed -n 's|postgres://\([^:]*\):.*|\1|p')
PG_PASS ?= $$(echo "$(DSN)" | sed -n 's|postgres://[^:]*:\([^@]*\)@.*|\1|p')
PG_HOST_PORT ?= $$(echo "$(DSN)" | sed -n 's|.*@\([^/]*\)/.*|\1|p')
TEST_DSN ?= postgres://$(PG_USER):$(PG_PASS)@$(PG_HOST_PORT)/$(TEST_DB_NAME)?sslmode=disable
PG_CONTAINER ?= marketplace-api-postgres-1
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

# backfill-previews — эмитит outbox-событие portfolio.video_uploaded для
# всех portfolio_items.preview_status='pending'. Нужно после миграции 00020
# чтобы для существующих видео сгенерился preview (новые делают эмит сами
# в profiles.AddPortfolioVideo). Idempotent — handler в worker'е игнорит
# уже processing/ready записи.
backfill-previews:
	go run ./cmd/backfill-previews

# Прод-вариант: запускает уже собранный binary в контейнере worker.
# Использовать после deploy'a с миграцией 00020.
prod-backfill-previews:
	docker compose -f docker-compose.prod.yml --env-file .env.prod run --rm worker backfill-previews

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

# === Integration test DB ===
# Поднимает отдельную БД marketpclce_test в том же postgres-контейнере,
# что и dev. Не трогает dev-БД. Идемпотентно — повторный вызов не падает.
test-db-up:
	@docker exec $(PG_CONTAINER) psql -U $(PG_USER) -d postgres -tAc \
		"SELECT 1 FROM pg_database WHERE datname='$(TEST_DB_NAME)'" | grep -q 1 \
		|| docker exec $(PG_CONTAINER) psql -U $(PG_USER) -d postgres -c \
			"CREATE DATABASE $(TEST_DB_NAME);"
	@TEST_DSN_FOR_MIGRATE="$(TEST_DSN)"; \
		go run -tags='$(GOOSE_TAGS)' github.com/pressly/goose/v3/cmd/goose@latest \
			-dir migrations postgres "$$TEST_DSN_FOR_MIGRATE" up
	@echo "test-db ready: $(TEST_DSN)"

# Сбрасывает test-БД — DROP + CREATE + миграции. Полная зачистка между прогонами.
test-db-reset:
	@docker exec $(PG_CONTAINER) psql -U $(PG_USER) -d postgres -c \
		"DROP DATABASE IF EXISTS $(TEST_DB_NAME);"
	@$(MAKE) test-db-up

# Запускает все тесты с TEST_DATABASE_URL — integration-тесты не SKIPped.
# Перед запуском убедись что test-db поднята (make test-db-up).
test-integration:
	TEST_DATABASE_URL="$(TEST_DSN)" go test ./...

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

# ── n8n volume backup ────────────────────────────────────────────────
# Бекап всего volume n8n: workflows + credentials + encryption-key + sqlite.
# Полный снапшот — годится для миграции на новый VDS и для DR.
# Без --stop: sqlite в WAL-режиме читается консистентно из работающего
# контейнера. Если хочется гарантий — `docker stop` перед вызовом.
#
# Перенос на другой VDS:
#   1) make backup-n8n          # на старом сервере → backups/n8n/n8n-*.tar.gz
#   2) scp backups/n8n/n8n-*.tar.gz new-vds:/opt/marketplace-api/backups/n8n/
#   3) на новом VDS: запустить контейнер n8n один раз (создаёт volume),
#      остановить, и `make restore-n8n` — затрёт пустой volume снапшотом.
N8N_CONTAINER ?= marketplace-api-n8n-1
N8N_VOLUME ?= marketplace-api_n8n-data
N8N_BACKUP_DIR ?= backups/n8n
backup-n8n:
	@mkdir -p $(N8N_BACKUP_DIR)
	@OUT="$(N8N_BACKUP_DIR)/n8n-$$(date +%Y-%m-%d_%H%M%S).tar.gz"; \
	 echo "[n8n-backup] $$OUT"; \
	 docker run --rm -v $(N8N_VOLUME):/data:ro -v "$$(pwd)/$(N8N_BACKUP_DIR):/out" alpine \
	   tar czf "/out/$$(basename $$OUT)" -C /data . ; \
	 SIZE=$$(stat -c%s "$$OUT" 2>/dev/null || stat -f%z "$$OUT"); \
	 if [ "$$SIZE" -lt 1024 ]; then \
	   echo "[n8n-backup] FAIL: $$OUT < 1KB"; rm -f "$$OUT"; exit 1; \
	 fi; \
	 echo "[n8n-backup] OK: $$(numfmt --to=iec $$SIZE 2>/dev/null || echo $$SIZE bytes)"; \
	 find $(N8N_BACKUP_DIR) -name "n8n-*.tar.gz" -type f -mtime +$(BACKUP_KEEP_DAYS) -print -delete

# Восстановление n8n volume из tarball. Затирает текущее содержимое
# volume — контейнер автоматически тушится/поднимается.
#   make restore-n8n                                  # последний
#   make restore-n8n BACKUP_FILE=backups/n8n/x.tar.gz # конкретный
restore-n8n:
	@FILE="$${BACKUP_FILE:-$$(ls -t $(N8N_BACKUP_DIR)/n8n-*.tar.gz 2>/dev/null | head -1)}"; \
	 if [ -z "$$FILE" ] || [ ! -f "$$FILE" ]; then \
	   echo "no n8n backup found in $(N8N_BACKUP_DIR)/, set BACKUP_FILE=..."; exit 1; \
	 fi; \
	 echo "[n8n-restore] $$FILE → volume $(N8N_VOLUME)"; \
	 docker stop $(N8N_CONTAINER) 2>/dev/null || echo "[n8n-restore] container not running"; \
	 docker run --rm -v $(N8N_VOLUME):/data -v "$$(pwd)/$$(dirname $$FILE):/in" alpine \
	   sh -c "find /data -mindepth 1 -delete 2>/dev/null; tar xzf /in/$$(basename $$FILE) -C /data"; \
	 docker start $(N8N_CONTAINER) 2>/dev/null && echo "[n8n-restore] OK, container restarted" || echo "[n8n-restore] start container manually"

# ── n8n workflow sync с git ──────────────────────────────────────────
# n8n-import — загрузить версионированные workflows из deploy/n8n/workflows/
# в n8n. Используется на первом старте VDS (после restore-n8n не нужен —
# tarball уже включает workflows). Идемпотентно: повторный импорт обновляет
# определения, но СБРАСЫВАЕТ active-флаг в false. Поэтому workflow'ы после
# импорта нужно вручную активировать в UI (тумблер Active).
#
# Credentials (Telegram bot token, UniSender API key, Postgres) — отдельно,
# через n8n UI: при импорте credential-references в нодах указывают на
# имя ('Telegram CRM bot' и т.п.), а юзер привязывает реальные credentials.
N8N_WORKFLOWS_DIR ?= deploy/n8n/workflows
n8n-import:
	@if [ ! -d "$(N8N_WORKFLOWS_DIR)" ]; then \
	  echo "no $(N8N_WORKFLOWS_DIR)/ — nothing to import"; exit 1; fi
	@echo "[n8n-import] $(N8N_WORKFLOWS_DIR)/ → $(N8N_CONTAINER)"
	@docker exec $(N8N_CONTAINER) sh -c 'rm -rf /tmp/wf-import && mkdir -p /tmp/wf-import'
	@docker cp $(N8N_WORKFLOWS_DIR)/. $(N8N_CONTAINER):/tmp/wf-import/
	@for f in $(N8N_WORKFLOWS_DIR)/*.json; do \
	   name=$$(basename $$f); \
	   echo "  → $$name"; \
	   docker exec $(N8N_CONTAINER) sh -c "printf '[' > /tmp/wf-one.json && cat /tmp/wf-import/$$name >> /tmp/wf-one.json && printf ']' >> /tmp/wf-one.json"; \
	   docker exec $(N8N_CONTAINER) n8n import:workflow --input=/tmp/wf-one.json 2>&1 | grep -v '^$$' | tail -2; \
	 done

# n8n-export — выгрузить workflows из n8n в deploy/n8n/workflows/ для
# коммита в git. Чистит per-install метаданные (createdAt, versionId,
# project ownership) и credential id'шники — оставляет только имя.
# Используется когда правил workflow в UI и хочешь зафиксировать в git.
n8n-export:
	@mkdir -p $(N8N_WORKFLOWS_DIR)
	@docker exec $(N8N_CONTAINER) sh -c 'rm -rf /tmp/wf-export && mkdir -p /tmp/wf-export && n8n export:workflow --all --separate --output=/tmp/wf-export' 2>&1 | tail -2
	@rm -rf /tmp/n8n-export-tmp && mkdir -p /tmp/n8n-export-tmp
	@docker cp $(N8N_CONTAINER):/tmp/wf-export/. /tmp/n8n-export-tmp/
	@for f in /tmp/n8n-export-tmp/*.json; do \
	   python3 -c "import json,sys; w=json.load(open('$$f')); clean={'id':w['id'],'name':w['name'],'active':False,'nodes':w['nodes'],'connections':w['connections'],'settings':w.get('settings',{'executionOrder':'v1'})}; \
[n.update({'credentials':{k:{'name':v.get('name','')} for k,v in n['credentials'].items()}}) if 'credentials' in n else None for n in clean['nodes']]; \
print(json.dumps(clean, ensure_ascii=False, indent=2))" > "$(N8N_WORKFLOWS_DIR)/$$(basename $$f)"; \
	   echo "  → $$(basename $$f)"; \
	 done
	@rm -rf /tmp/n8n-export-tmp

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

# ── Grafana Cloud: дашборды и алерты через API ──────────────────────
# Авторизация:
# - GRAFANA_URL — https://<твой-стек>.grafana.net (из cabinet → My Account)
# - GRAFANA_TOKEN — Service Account token с правами:
#     • Dashboards: Admin (для grafana-dashboards-push)
#     • Rules: Editor (для grafana-rules-push)
#   Создать: Grafana Cloud UI → Administration → Service accounts → New.
# - Для rules: нужен GRAFANA_CLOUD_PROM_USER + GRAFANA_CLOUD_PROM_URL
#   (тот же что в .env.prod для alloy push) — Mimir-API живёт на этом же
#   домене. Имя rule-group берётся из YAML (groups[].name).
GRAFANA_URL ?= $$(grep -E '^GRAFANA_URL=' .env.prod 2>/dev/null | cut -d= -f2-)
GRAFANA_TOKEN ?= $$(grep -E '^GRAFANA_TOKEN=' .env.prod 2>/dev/null | cut -d= -f2-)
GRAFANA_PROM_URL ?= $$(grep -E '^GRAFANA_CLOUD_PROM_URL=' .env.prod 2>/dev/null | cut -d= -f2-)
GRAFANA_PROM_USER ?= $$(grep -E '^GRAFANA_CLOUD_PROM_USER=' .env.prod 2>/dev/null | cut -d= -f2-)
GRAFANA_PROM_KEY ?= $$(grep -E '^GRAFANA_CLOUD_PROM_KEY=' .env.prod 2>/dev/null | cut -d= -f2-)

# Залить все дашборды из grafana/*-dashboard.json в Grafana Cloud.
# Использует POST /api/dashboards/db с overwrite=true — обновит существующий
# по uid'у, либо создаст новый. Папка default — корневая.
grafana-dashboards-push:
	@test -n "$(GRAFANA_URL)" || (echo "GRAFANA_URL пуст. Заполни в .env.prod"; exit 1)
	@test -n "$(GRAFANA_TOKEN)" || (echo "GRAFANA_TOKEN пуст. Создай Service Account → Generate token"; exit 1)
	@for f in grafana/*-dashboard.json; do \
	  name=$$(basename $$f .json); \
	  echo "[grafana] → $$name"; \
	  body=$$(jq -n --slurpfile d $$f '{dashboard: $$d[0], overwrite: true, folderId: 0}'); \
	  resp=$$(curl -s -w "\n%{http_code}" -X POST "$(GRAFANA_URL)/api/dashboards/db" \
	    -H "Authorization: Bearer $(GRAFANA_TOKEN)" \
	    -H "Content-Type: application/json" \
	    -d "$$body"); \
	  code=$$(echo "$$resp" | tail -1); \
	  if [ "$$code" = "200" ]; then \
	    url=$$(echo "$$resp" | head -n-1 | jq -r '.url // ""'); \
	    echo "  ✓ $(GRAFANA_URL)$$url"; \
	  else \
	    echo "  ✗ HTTP $$code: $$(echo "$$resp" | head -n-1)"; exit 1; \
	  fi; \
	done

# Залить grafana/alerts.yml в Grafana Cloud Mimir. Требует mimirtool:
#   brew install grafana/grafana/mimirtool
# или: go install github.com/grafana/mimir/cmd/mimirtool@latest
#
# Mimir Rules API:
#   POST {prom_url%/api/prom/push}/api/v1/rules/{namespace}
#   namespace = "marketpclce" (единый для всех групп из alerts.yml)
grafana-rules-push:
	@test -n "$(GRAFANA_PROM_URL)" || (echo "GRAFANA_CLOUD_PROM_URL пуст в .env.prod"; exit 1)
	@test -n "$(GRAFANA_PROM_USER)" || (echo "GRAFANA_CLOUD_PROM_USER пуст"; exit 1)
	@test -n "$(GRAFANA_PROM_KEY)" || (echo "GRAFANA_CLOUD_PROM_KEY пуст"; exit 1)
	@command -v mimirtool >/dev/null || (echo "mimirtool не установлен. См. https://grafana.com/docs/mimir/latest/manage/tools/mimirtool/"; exit 1)
	@PROM_BASE=$$(echo "$(GRAFANA_PROM_URL)" | sed 's|/api/prom/push||'); \
	 echo "[mimir] sync rules → $$PROM_BASE"; \
	 mimirtool rules sync \
	   --address="$$PROM_BASE" \
	   --id="$(GRAFANA_PROM_USER)" \
	   --key="$(GRAFANA_PROM_KEY)" \
	   grafana/alerts.yml
