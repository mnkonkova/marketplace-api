#!/usr/bin/env bash
# Zero-downtime redeploy: пуллит оба репо, билдит образы ЗАРАНЕЕ (старые
# контейнеры в это время работают), потом атомарно пересоздаёт api/worker/web
# через graceful shutdown. Caddy фронта при этом ретраит /api/*, поэтому
# пользователь видит «медленный» запрос (1–3с), а не 502.
#
# Когда применять:
#   • обычный апдейт кода (Go или фронт) — обычный поток.
#   • НЕ применять, если в этом релизе несовместимые миграции БД (drop колонок,
#     rename таблиц): тогда сначала сделай миграцию + бэк, потом фронт.
#     Для аддитивных миграций (add column, add table) — ок, redeploy справится.
#
# Использование:
#   ./scripts/redeploy.sh            # full: оба репо
#   ./scripts/redeploy.sh api        # только API/worker (git pull api + rebuild Go)
#   ./scripts/redeploy.sh web        # только фронт
#   SKIP_PULL=1 ./scripts/redeploy.sh   # без git pull (если изменения уже на месте)
#   SKIP_MIGRATE=1 ./scripts/redeploy.sh  # без goose up

set -euo pipefail
cd "$(dirname "$0")/.."

target="${1:-all}"
case "$target" in
  all|api|web) ;;
  *) echo "ERR: unknown target '$target'. Use: all | api | web" >&2; exit 1 ;;
esac

if [[ ! -f .env.prod ]]; then
  echo "ERR: .env.prod missing." >&2; exit 1
fi
if [[ ! -d ../web ]]; then
  echo "ERR: ../web missing. git clone marketplace-web /opt/marketpclce/web" >&2; exit 1
fi

COMPOSE=(docker compose -f docker-compose.prod.yml --env-file .env.prod)

# 1. git pull обоих (или только нужного). --ff-only — отвалит,
#    если на сервере были локальные правки: лучше падать рано.
if [[ "${SKIP_PULL:-0}" != "1" ]]; then
  if [[ "$target" == "all" || "$target" == "api" ]]; then
    echo "→ git pull api"
    git pull --ff-only
  fi
  if [[ "$target" == "all" || "$target" == "web" ]]; then
    echo "→ git pull web"
    git -C ../web pull --ff-only
  fi
fi

# 2. Build ЗАРАНЕЕ, пока старые контейнеры работают. Compose v2 билдит
#    несколько сервисов параллельно. Это самая долгая фаза — но downtime'а
#    в ней нет, наружу всё ещё отвечает старый api/web.
echo "→ build images (старые контейнеры пока работают)"
case "$target" in
  all) "${COMPOSE[@]}" build api web ;;
  api) "${COMPOSE[@]}" build api ;;
  web) "${COMPOSE[@]}" build web ;;
esac

# 3. Source .env.prod для DATABASE_URL (нужен goose) — только если будем
#    мигрировать.
if [[ "${SKIP_MIGRATE:-0}" != "1" && ( "$target" == "all" || "$target" == "api" ) ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env.prod
  set +a

  echo "→ apply migrations (idempotent)"
  "${COMPOSE[@]}" run --rm api goose -dir /app/migrations postgres "$DATABASE_URL" up
fi

# 4. Recreate. up -d посылает SIGTERM → Go API ловит, дослуживает активные
#    запросы (HTTPShutdownTimeout=15s), exit'ит. Новый контейнер стартует.
#    Caddy ретраит — пользователь не видит 502.
echo "→ recreate containers"
case "$target" in
  all)
    "${COMPOSE[@]}" up -d --no-deps api worker
    "${COMPOSE[@]}" up -d --no-deps web
    ;;
  api)
    "${COMPOSE[@]}" up -d --no-deps api worker
    ;;
  web)
    "${COMPOSE[@]}" up -d --no-deps web
    ;;
esac

# 5. Удалить старые/висячие образы — экономит место на диске VDS.
echo "→ prune dangling images"
docker image prune -f >/dev/null

# 6. Health-check, чтобы deploy не молчал о факапе.
echo "→ health-check"
sleep 2
"${COMPOSE[@]}" ps
if ! "${COMPOSE[@]}" exec -T api wget -qO- http://localhost:8080/healthz >/dev/null 2>&1; then
  echo "WARN: /healthz не отвечает изнутри контейнера. Логи:"
  "${COMPOSE[@]}" logs --tail=30 api
  exit 1
fi
echo "✓ done"
