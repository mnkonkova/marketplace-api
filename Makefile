.PHONY: up down logs ps run build tidy migrate-up migrate-down migrate-status migrate-create test lint fmt

DC ?= docker compose
DSN ?= $$(grep -E '^DATABASE_URL=' .env 2>/dev/null | cut -d= -f2- | tr -d '"')

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
