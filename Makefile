.PHONY: up down logs ps run build tidy migrate-up migrate-down migrate-status migrate-create test lint fmt swag \
        deploy redeploy redeploy-api redeploy-web prod-up prod-down prod-logs prod-ps prod-build prod-migrate prod-seed

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

migrate-up:
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DSN)" up

migrate-down:
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DSN)" down

migrate-status:
	go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$(DSN)" status

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
