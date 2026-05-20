#!/usr/bin/env bash
# Идемпотентный деплой на VDS. Запускать из корня API-репо:
#   ./scripts/deploy.sh   (или make deploy)
# Перед первым запуском:
#   • заполнить .env.prod (см. .env.prod.example)
#   • рядом склонировать marketplace-web как ../web
#     (compose билдит фронт из ../web — см. docker-compose.prod.yml).

set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env.prod ]]; then
  echo "ERR: .env.prod missing. Copy .env.prod.example → .env.prod and fill it." >&2
  exit 1
fi

if [[ ! -d ../web ]]; then
  echo "ERR: ../web missing. Run from the parent dir:" >&2
  echo "  git clone https://github.com/mnkonkova/marketplace-web ../web" >&2
  exit 1
fi

# Pull обоих репо: api (текущий) и web (соседний). Снять через
# SKIP_PULL=1 ./scripts/deploy.sh, если CI уже сделал checkout.
if [[ "${SKIP_PULL:-0}" != "1" ]]; then
  echo "→ git pull api"
  git pull --ff-only
  echo "→ git pull web"
  git -C ../web pull --ff-only
fi

# Пробрасываем переменные .env.prod в окружение скрипта (нужны для
# pg_isready и goose-команды ниже). docker compose читает .env.prod
# отдельно через --env-file.
set -a
# shellcheck disable=SC1091
source .env.prod
set +a

COMPOSE=(docker compose -f docker-compose.prod.yml --env-file .env.prod)

echo "→ build images (api + web)"
"${COMPOSE[@]}" build api web

echo "→ start data services (postgres / opensearch / redis)"
"${COMPOSE[@]}" up -d postgres opensearch redis

echo "→ wait for postgres healthy"
until "${COMPOSE[@]}" exec -T postgres pg_isready -U "${POSTGRES_USER:-marketpclce}" -d "${POSTGRES_DB:-marketpclce}" >/dev/null 2>&1; do
  sleep 1
done

echo "→ apply migrations"
"${COMPOSE[@]}" run --rm api goose -dir /app/migrations postgres "$DATABASE_URL" up

echo "→ (re)start app services"
"${COMPOSE[@]}" up -d --no-deps --build api worker
"${COMPOSE[@]}" up -d --no-deps --build web

echo "→ prune dangling images"
docker image prune -f

echo "✓ done"
"${COMPOSE[@]}" ps
