#!/usr/bin/env bash
# n8n-export.sh — выгружает все workflows из n8n в JSON-файлы.
# Использование:
#   N8N_API_KEY=<key> ./scripts/n8n-export.sh http://localhost:5678 ops/n8n/workflows
#
# Каждый workflow сохраняется в файл <name-slug>.json. Поля runtime
# (id, createdAt, updatedAt, active, isArchived) очищаются — workflow
# становится «портативным» (импортируется как новый или обновляет
# существующий с тем же именем).
set -euo pipefail

N8N_URL="${1:-http://localhost:5678}"
OUT_DIR="${2:-ops/n8n/workflows}"
: "${N8N_API_KEY:?N8N_API_KEY is required (Settings → n8n API → Create API Key)}"

mkdir -p "$OUT_DIR"
cd "$OUT_DIR"

# Список workflow'ов.
LIST=$(curl -sS -H "X-N8N-API-KEY: $N8N_API_KEY" "$N8N_URL/api/v1/workflows")

# Перебираем id+name.
echo "$LIST" | python3 -c "
import sys, json
d = json.load(sys.stdin).get('data', [])
for wf in d:
    print(wf['id'], wf['name'])
" | while read -r id name; do
  slug=$(echo "$name" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g' | sed 's/--*/-/g' | sed 's/^-\|-$//g')
  file="${slug}.json"
  echo "→ $file ($name)"
  curl -sS -H "X-N8N-API-KEY: $N8N_API_KEY" "$N8N_URL/api/v1/workflows/$id" \
    | python3 -c "
import sys, json
wf = json.load(sys.stdin)
# Очищаем runtime-поля; оставляем то, что нужно для импорта.
for k in ['id', 'createdAt', 'updatedAt', 'active', 'isArchived', 'shared', 'versionId', 'triggerCount', 'meta']:
    wf.pop(k, None)
print(json.dumps(wf, indent=2, ensure_ascii=False))
" > "$file"
done

echo ""
echo "Exported $(ls -1 *.json 2>/dev/null | wc -l) workflows to $(pwd)"
