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
  "event_id":     "<id строки outbox, число строкой>",
  "aggregate":    "project",
  "aggregate_id": "<project uuid>",
  "event_type":   "project.step_transitioned",
  "data":         { ... оригинальный outbox payload ... },
  "occurred_at":  "2026-06-01T10:00:00Z"
}
```

`Authorization: Bearer <N8N_WEBHOOK_TOKEN>` если токен задан.

## Типы событий

Полный список — в `internal/eventroute/chat.go`: там каждый тип объявлен
явно и помечен «идёт в чат» или «намеренно не идёт». Копии списка здесь
больше нет намеренно: она врала. Шесть строк в этой таблице выглядели
как «вот что мы шлём», а шлём мы почти сорок типов — остальные тридцать
с лишним приходили в n8n и молча выбрасывались веткой `default`.

Таблицу в коде сторожат два теста (`internal/eventroute`): первый не даёт
завести тип события мимо неё, второй сверяет набор «идёт в чат» с
ветками `deploy/n8n/workflows/crmTgEventsV1.json`. Разошлись — падает
сборка, а не тихо теряется сообщение.

**Правило, по которому тип попадает в чат:** в чат идёт только то, на
что нужно ответить человеком и чего он иначе не увидит вовремя.
Остальное живёт в CRM — там есть сводка «Требует внимания», и
дублировать её в чат значит превратить чат в ленту, которую перестанут
читать.

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

`event_id` — это id строки outbox, уникальный по таблице.

**Дедупа сейчас нет ни с одной стороны, и это не теория.** Воркер берёт
запись в аренду, отправляет HTTP вне транзакции и лишь затем помечает
результат: падение или SIGTERM между отправкой и пометкой возвращает
запись в работу через 10 минут, и событие уходит вторым разом. На нашей
стороне это сознательно не лечится — дедуп делегирован n8n. Но в
экспортированных workflow (`deploy/n8n/workflows/*.json`) `event_id` не
используется нигде, то есть делегировали в пустоту.

Чтобы гарантия появилась: Workflow Settings → Caller policy =
`Reject duplicates by event_id` (либо вручную через Data Store).
