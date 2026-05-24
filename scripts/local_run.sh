#!/usr/bin/env bash
# Локальный запуск API + worker. Из корня репо:
#   ./scripts/local_run.sh
# Поднимает postgres/opensearch/redis в docker, ждёт pg, накатывает миграции,
# запускает API (:8080) и outbox-worker. Ctrl+C — оба процесса гасятся,
# контейнеры остаются (down делать вручную: make down).
#
# Флаги через ENV:
#   SKIP_UP=1       — не дергать docker compose up
#   SKIP_MIGRATE=1  — не запускать goose
#   SKIP_WORKER=1   — только API, без worker'а
#   SEED=1          — после миграций прогнать `make seed`

set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "ERR: .env missing. Copy .env.example → .env and fill JWT_SECRET / DATABASE_URL." >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
source .env
set +a

: "${DATABASE_URL:?DATABASE_URL not set in .env}"

DC=(docker compose)

if [[ "${SKIP_UP:-0}" != "1" ]]; then
  echo "→ docker compose up (postgres + opensearch + redis)"
  "${DC[@]}" up -d postgres opensearch redis

  echo "→ wait for postgres healthy"
  until "${DC[@]}" exec -T postgres pg_isready -U "${POSTGRES_USER:-marketpclce}" -d "${POSTGRES_DB:-marketpclce}" >/dev/null 2>&1; do
    sleep 1
  done

  # OpenSearch стартует медленнее, чем pg — worker без него падает с
  # os.Exit(1) на EnsureIndex. Ждём пока кластер ответит хотя бы yellow.
  os_url="${OPENSEARCH_URL:-http://localhost:9200}"
  echo "→ wait for opensearch ready (${os_url})"
  until curl -sf "${os_url}/_cluster/health?wait_for_status=yellow&timeout=5s" >/dev/null 2>&1; do
    sleep 1
  done
fi

if [[ "${SKIP_MIGRATE:-0}" != "1" ]]; then
  echo "→ goose up"
  go run github.com/pressly/goose/v3/cmd/goose@latest -dir migrations postgres "$DATABASE_URL" up
fi

if [[ "${SEED:-0}" == "1" ]]; then
  echo "→ seed"
  go run ./cmd/seed
fi

pids=()
cleanup() {
  echo
  echo "→ stopping local processes"
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup INT TERM EXIT

echo "→ start API (${HTTP_ADDR:-:8080})"
go run ./cmd/api &
pids+=($!)

if [[ "${SKIP_WORKER:-0}" != "1" ]]; then
  echo "→ start worker"
  go run ./cmd/worker &
  pids+=($!)
fi

wait
