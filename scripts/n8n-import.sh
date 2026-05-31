#!/usr/bin/env bash
# n8n-import.sh — загружает workflows из ops/n8n/workflows в n8n.
# Идемпотентность: если workflow с таким же именем уже есть — обновляет
# его (PATCH); иначе создаёт новый (POST). После загрузки активирует.
#
# Использование:
#   N8N_API_KEY=<key> ./scripts/n8n-import.sh http://localhost:5678 ops/n8n/workflows
set -euo pipefail

N8N_URL="${1:-http://localhost:5678}"
DIR="${2:-ops/n8n/workflows}"
: "${N8N_API_KEY:?N8N_API_KEY is required}"

# Карта name → id из текущего n8n.
EXISTING=$(curl -sS -H "X-N8N-API-KEY: $N8N_API_KEY" "$N8N_URL/api/v1/workflows" \
  | python3 -c "
import sys, json
d = json.load(sys.stdin).get('data', [])
for wf in d:
    print(wf['name'] + '\t' + wf['id'])
")

for file in "$DIR"/*.json; do
  [ -f "$file" ] || continue
  name=$(python3 -c "import sys, json; print(json.load(open('$file'))['name'])")
  existing_id=$(echo "$EXISTING" | awk -F'\t' -v n="$name" '$1==n {print $2}')

  if [ -n "$existing_id" ]; then
    echo "→ UPDATE '$name' (id=$existing_id)"
    curl -sS -X PUT "$N8N_URL/api/v1/workflows/$existing_id" \
      -H "X-N8N-API-KEY: $N8N_API_KEY" -H "Content-Type: application/json" \
      -d "@$file" > /dev/null
    WF_ID="$existing_id"
  else
    echo "→ CREATE '$name'"
    WF_ID=$(curl -sS -X POST "$N8N_URL/api/v1/workflows" \
      -H "X-N8N-API-KEY: $N8N_API_KEY" -H "Content-Type: application/json" \
      -d "@$file" \
      | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
  fi

  # Активируем (no-op если уже активен).
  curl -sS -X POST "$N8N_URL/api/v1/workflows/$WF_ID/activate" \
    -H "X-N8N-API-KEY: $N8N_API_KEY" > /dev/null
  echo "  activated"
done

echo ""
echo "Done."
