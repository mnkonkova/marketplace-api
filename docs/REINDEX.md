# Reindex OpenSearch при смене маппинга

## Когда это нужно

OpenSearch не позволяет менять `settings.analysis` (анализаторы, фильтры,
синонимы) на живом индексе. Если ты меняешь `mapping.go` или
`feed_mapping.go` — новые запросы будут использовать старый анализатор,
пока индекс не пересоздан.

Признаки: поменял синонимы, деплой прошёл, а поиск не находит новые
варианты (например, «тикток» не находит спецов с скилом `tiktok`).

## Как переиндексировать

1. В `.env.prod` (или где хранишь env worker'a):
   ```
   OPENSEARCH_REINDEX_ON_START=true
   ```
2. Задеплой worker (обычно `make redeploy-worker` или через compose).
3. Смотри логи — должно быть:
   ```
   OPENSEARCH_REINDEX_ON_START=true — DROP both indices, will rebuild from scratch
   ensure index ok after retry index=specialists
   ensure index ok after retry index=feed_videos
   specialists bootstrapped total=N indexed=N
   feed_videos bootstrapped specialists=N
   ```
4. Верни в `.env.prod`:
   ```
   OPENSEARCH_REINDEX_ON_START=false
   ```
5. Задеплой worker ещё раз (иначе при следующем рестарте индексы снесутся снова).

## Даунтайм

Пока worker бутстрапится:
- `/api/v1/search` — вернёт пустоту или частичный результат.
- `/api/v1/feed` — то же самое.
- `/api/v1/specialists/{id}` — работает (читает из PG напрямую, не из ES).
- Регистрация / логин / профиль — работают.

Оценка длительности на MVP-объёме (~100 спецов, ~500 видео):
- delete + create indices: 1-2 сек.
- bootstrap specialists Reconcile: ~5-10 сек.
- bootstrap feed_videos ReconcileVideos: ~10-30 сек.
- **Итого: ~15-45 сек**.

**Делай ночью** (0:00-6:00 MSK) когда трафик минимален.

## Если что-то пошло не так

1. Индексы удалились, но bootstrap упал (например, PG недоступен) —
   индексы остаются пустыми. Поиск отдаёт пусто.
2. Fix: убедись что PG жив, restart worker без `OPENSEARCH_REINDEX_ON_START`
   (bootstrap запустится по флагу `IsEmpty()` автоматически).

## Мониторинг

В Grafana в `Marketplace API - core` дашборде смотри `specialists_index_size`
и `feed_videos_index_size` — оба должны быть > 0 после bootstrap'а.
