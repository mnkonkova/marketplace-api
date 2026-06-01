# n8n workflows для CRM v5

Воркер (`cmd/worker`) шлёт POST на `N8N_WEBHOOK_URL` для всех событий
агрегата `project` (см. `internal/outbox/emit.go`). Конфиг в `.env.prod`:

```
N8N_WEBHOOK_URL=http://n8n:5678/webhook/crm-events
N8N_WEBHOOK_TOKEN=<секрет>
```

Если переменная пустая — диспатч выключен, события успешно квитируются
как no-op (не зависают в outbox-ретраях).

## Формат payload

```json
{
  "event_id":     "<aggregate_id>/<event_type>",
  "aggregate":    "project",
  "aggregate_id": "<project uuid>",
  "event_type":   "project.step_transitioned",
  "data":         { ... оригинальный outbox payload ... },
  "occurred_at":  "2026-06-01T10:00:00Z"
}
```

`Authorization: Bearer <N8N_WEBHOOK_TOKEN>` если токен задан.

## Типы событий

| event_type                       | Когда                                          |
|----------------------------------|------------------------------------------------|
| `project.created`                | StartProject                                   |
| `project.step_transitioned`      | Любой переход шага (включая system auto-skip)  |
| `project.stage_advanced`         | advance_stage менеджером/админом               |
| `project.assigned`               | Manager claim                                  |
| `project.disputed`               | Лимит правок исчерпан                          |
| `project.completed`              | Все шаги done/skipped                          |

## Workflows (заготовки)

1. **project-client-notification** — слать клиенту email при переходе
   его шага в `waiting_client+owner=client`. Триггер: webhook → IF
   `event_type=="project.step_transitioned" AND data.to=="waiting_client"`
   → HTTP-нода для получения профиля клиента → Send Email (Unisender).

2. **client-invite** — на `client_invite.generated` (отдельный event_type,
   если потом добавим) или сразу из admin-эндпоинта — email с magic-link.

3. **manager-notification** — на `project.disputed` слать в Telegram-канал
   команды (с inline-кнопками для быстрого вмешательства).

Файлы JSON-экспорта n8n кладите рядом (`*.json`) — импортируются через
UI или CLI.

## Идемпотентность

`event_id` уникален в пределах outbox-таблицы (`aggregate_id/event_type`
+ внутренний sequence id). На стороне n8n настройте Workflow Settings →
Caller policy = `Reject duplicates by event_id` (либо вручную в Data
Store).
