#!/usr/bin/env bash
# Идемпотентная настройка Directus-коллекций для воронок (funnel templates).
# Запускать при первом подъёме стенда или после миграции 00011_crm_projects.
#
# Что делает:
#  1) Логинится в Directus админом.
#  2) Ставит collection-meta (иконки, display_template, translations RU/EN,
#     sort_field, archive_field) для service_templates/_stages/_steps.
#  3) Ставит field-meta для всех полей (interface, dropdown'ы, hidden,
#     требуемость, подсказки).
#  4) Раздаёт permissions: platform_admin = CRUD, platform_moderator = read.
#  5) Сбрасывает Directus cache.
#
# Идемпотентно: PATCH'и на meta перезаписывают существующее; POST'ы на
# permissions дублируются — нестрашно для admin-policy (Directus
# ignored-duplicates), но в идеале вычищать перед накатом. Этот скрипт
# сначала удаляет существующие permissions для трёх коллекций и
# накатывает заново.
#
# Использование (локально):
#   DIRECTUS_URL=http://localhost:8055 \
#   DIRECTUS_ADMIN_EMAIL=admin@example.com \
#   DIRECTUS_ADMIN_PASSWORD=Admin12345 \
#   ADMIN_POLICY_ID=<id из настройки ролей> \
#   MOD_POLICY_ID=<id из настройки ролей> \
#   ./ops/directus/setup-funnels.sh
#
# Прод: те же переменные, но DIRECTUS_URL=https://admin.{DOMAIN}.
set -euo pipefail

DIRECTUS_URL="${DIRECTUS_URL:-http://localhost:8055}"
: "${DIRECTUS_ADMIN_EMAIL:?DIRECTUS_ADMIN_EMAIL required}"
: "${DIRECTUS_ADMIN_PASSWORD:?DIRECTUS_ADMIN_PASSWORD required}"
: "${ADMIN_POLICY_ID:?ADMIN_POLICY_ID required (см. docs/ADMIN_DIRECTUS.md, секция «Настройка ролей»)}"
: "${MOD_POLICY_ID:?MOD_POLICY_ID required}"

# Логин и получение access-token. python нужен для парсинга JSON —
# он есть и в локалке, и в alpine-контейнерах (через apk add python3).
TOKEN=$(curl -fsS -X POST "$DIRECTUS_URL/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$DIRECTUS_ADMIN_EMAIL\",\"password\":\"$DIRECTUS_ADMIN_PASSWORD\"}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['access_token'])")
AUTH="Authorization: Bearer $TOKEN"
CT="Content-Type: application/json"

ok() { python3 -c "import sys,json; d=json.load(sys.stdin); print('  ok' if 'data' in d or d=={} else d)"; }

# ─── Шаг 1: collection-meta ──────────────────────────────────────────
echo "→ Collection metas (3 collections)"

curl -fsS -X PATCH "$DIRECTUS_URL/collections/service_templates" -H "$AUTH" -H "$CT" -d '{
  "meta":{
    "icon":"movie_filter",
    "note":"Шаблоны воронок (funnel templates). Редактируешь — изменения вступают для НОВЫХ проектов; уже запущенные используют snapshot и не меняются.",
    "display_template":"{{title}} v{{version}}",
    "sort_field":"code",
    "hidden":false,
    "translations":[
      {"language":"en-US","translation":"Funnel Templates"},
      {"language":"ru-RU","translation":"Воронки"}
    ]
  }
}' | ok

curl -fsS -X PATCH "$DIRECTUS_URL/collections/service_template_stages" -H "$AUTH" -H "$CT" -d '{
  "meta":{
    "icon":"layers",
    "note":"Стадии внутри воронки. Sort_order задаёт порядок.",
    "display_template":"{{title}}",
    "sort_field":"sort_order",
    "hidden":false,
    "translations":[
      {"language":"en-US","translation":"Funnel Stages"},
      {"language":"ru-RU","translation":"Стадии воронок"}
    ]
  }
}' | ok

curl -fsS -X PATCH "$DIRECTUS_URL/collections/service_template_steps" -H "$AUTH" -H "$CT" -d '{
  "meta":{
    "icon":"check_circle_outline",
    "note":"Шаги внутри стадии. owner=client → требует действия клиента; team → команда; system → автоматика. visible_to_client/specialist скрывает шаг от соответствующего ЛК.",
    "display_template":"{{title}} ({{owner}})",
    "sort_field":"sort_order",
    "hidden":false,
    "translations":[
      {"language":"en-US","translation":"Funnel Steps"},
      {"language":"ru-RU","translation":"Шаги воронок"}
    ]
  }
}' | ok

# ─── Шаг 2: field-meta ───────────────────────────────────────────────
echo "→ Field metas"

patch_field() {
  local table=$1; local field=$2; local body=$3
  curl -fsS -X PATCH "$DIRECTUS_URL/fields/$table/$field" -H "$AUTH" -H "$CT" -d "$body" > /dev/null
}

# service_templates
patch_field service_templates id \
  '{"meta":{"interface":"input","readonly":true,"hidden":true,"special":["uuid"],"sort":1}}'
patch_field service_templates code \
  '{"meta":{"interface":"input","required":true,"sort":2,"width":"half","note":"snake_case, например video_production. Уникален + version."}}'
patch_field service_templates version \
  '{"meta":{"interface":"input","required":true,"sort":3,"width":"half","note":"Целое. При смене шаблона создавай новую запись с version+1, не правь существующую (живые проекты будут работать со старой версией через snapshot)."}}'
patch_field service_templates title \
  '{"meta":{"interface":"input","required":true,"sort":4,"width":"full","note":"Человеческое название. Видно в дропдауне при создании проекта."}}'
patch_field service_templates is_active \
  '{"meta":{"interface":"boolean","sort":5,"width":"half","note":"Снять = воронка не появится в дропдауне. Уже запущенные проекты не задеты."}}'
patch_field service_templates revisions_included \
  '{"meta":{"interface":"input","sort":6,"width":"half","note":"Сколько раундов правок включено в стоимость (бизнес-правило)."}}'
patch_field service_templates created_at \
  '{"meta":{"interface":"datetime","readonly":true,"hidden":true,"special":["date-created"],"sort":7}}'

# service_template_stages
patch_field service_template_stages id \
  '{"meta":{"interface":"input","readonly":true,"hidden":true,"special":["uuid"],"sort":1}}'
patch_field service_template_stages template_id \
  '{"meta":{"interface":"select-dropdown-m2o","required":true,"sort":2,"width":"full","options":{"template":"{{title}} v{{version}}"},"display":"related-values","display_options":{"template":"{{title}} v{{version}}"}}}'
patch_field service_template_stages code \
  '{"meta":{"interface":"input","required":true,"sort":3,"width":"half","note":"snake_case, уникален в рамках template."}}'
patch_field service_template_stages title \
  '{"meta":{"interface":"input","required":true,"sort":4,"width":"half"}}'
patch_field service_template_stages sort_order \
  '{"meta":{"interface":"input","required":true,"sort":5,"width":"half","note":"Порядок отображения; 1, 2, 3..."}}'

# service_template_steps
patch_field service_template_steps id \
  '{"meta":{"interface":"input","readonly":true,"hidden":true,"special":["uuid"],"sort":1}}'
patch_field service_template_steps stage_id \
  '{"meta":{"interface":"select-dropdown-m2o","required":true,"sort":2,"width":"full","options":{"template":"{{title}}"},"display":"related-values","display_options":{"template":"{{title}}"}}}'
patch_field service_template_steps code \
  '{"meta":{"interface":"input","required":true,"sort":3,"width":"half","note":"snake_case, уникален в рамках stage. Используется кодом для конкретных правил (client_review триггерит approve-кнопку, final_cut переоткрывается на revision и т.д.). См. internal/projects/client_actions.go."}}'
patch_field service_template_steps title \
  '{"meta":{"interface":"input","required":true,"sort":4,"width":"half"}}'
patch_field service_template_steps owner \
  '{"meta":{"interface":"select-dropdown","required":true,"sort":5,"width":"half","options":{"choices":[{"text":"Клиент","value":"client"},{"text":"Команда","value":"team"},{"text":"Система (авто)","value":"system"}]}}}'
patch_field service_template_steps duration_days \
  '{"meta":{"interface":"input","required":true,"sort":6,"width":"half","note":"Сколько дней по плану. eta_date в проекте = today + duration_days при переходе in_progress."}}'
patch_field service_template_steps visible_to_client \
  '{"meta":{"interface":"boolean","sort":7,"width":"half","note":"Видит ли клиент этот шаг в своём ЛК. social_setup и internal_approval по дефолту скрыты от клиента."}}'
patch_field service_template_steps visible_to_specialist \
  '{"meta":{"interface":"boolean","sort":8,"width":"half","note":"Видит ли назначенный специалист. payment/nps/review скрыты от специалиста."}}'
patch_field service_template_steps weight \
  '{"meta":{"interface":"input","required":true,"sort":9,"width":"half","note":"Вес шага в расчёте прогресса. Большие шаги (черновой монтаж, 7 дней) делают тяжелее (weight=7)."}}'
patch_field service_template_steps sort_order \
  '{"meta":{"interface":"input","required":true,"sort":10,"width":"half","note":"Порядок внутри стадии."}}'

# ─── Шаг 3: permissions ───────────────────────────────────────────────
echo "→ Permissions (clean + reapply)"

# Чтобы скрипт был идемпотентным — сначала удаляем существующие
# permissions для наших трёх коллекций под обеими политиками.
for COLL in service_templates service_template_stages service_template_steps; do
  for POL in "$ADMIN_POLICY_ID" "$MOD_POLICY_ID"; do
    IDS=$(curl -fsS -g -H "$AUTH" \
      "$DIRECTUS_URL/permissions?filter[policy][_eq]=$POL&filter[collection][_eq]=$COLL&fields=id" \
      | python3 -c "import sys,json; print(' '.join(str(p['id']) for p in json.load(sys.stdin)['data']))")
    for ID in $IDS; do
      curl -fsS -X DELETE -H "$AUTH" "$DIRECTUS_URL/permissions/$ID" > /dev/null
    done
  done
done

add_perm() {
  local policy=$1; local coll=$2; local action=$3
  curl -fsS -X POST "$DIRECTUS_URL/permissions" -H "$AUTH" -H "$CT" \
    -d "{\"policy\":\"$policy\",\"collection\":\"$coll\",\"action\":\"$action\",\"fields\":[\"*\"]}" > /dev/null
}

for COLL in service_templates service_template_stages service_template_steps; do
  # platform_admin: CRUD
  for ACT in create read update delete; do
    add_perm "$ADMIN_POLICY_ID" "$COLL" "$ACT"
  done
  # platform_moderator: read-only
  add_perm "$MOD_POLICY_ID" "$COLL" "read"
done

# ─── Шаг 4: cache clear ───────────────────────────────────────────────
echo "→ Clearing Directus cache"
curl -fsS -X POST -H "$AUTH" "$DIRECTUS_URL/utils/cache/clear" > /dev/null

echo "✓ Done. Hard-refresh browser (Ctrl+Shift+R) to see changes."
