# Fix: исправление статусов и UI ЛК клиента

(Полный текст брифа сохранён здесь как источник истины. См. сообщение
пользователя от 2026-06-01, инициировавшее эту задачу. Ключевые
секции — модель данных, computed-функции, UI-таблица шагов, фазы
и DoD — приведены ниже.)

## Корень проблемы

ЛК клиента показывает захардкоженные строки («Скоро начнётся»,
«N в работе»), не связанные с реальным состоянием шагов. Проект на
77% по-прежнему «скоро начнётся». Фикс: статус вычисляется на бэке,
фронт только рендерит готовые поля.

## Backend (Phase 1)

Новые enum'ы:

- `ProjectDisplayStatus`: not_started | in_progress | waiting_action |
  completed | on_hold | cancelled.
- `StageDisplayStatus`: not_started | active | completed.

Новые поля:

- `ProjectClientView.display_status`, `progress` (0..100),
  `current_step_id`, `current_step_title`, `revisions_used`,
  `revisions_total`, `stages`.
- `StageView.display_status`, `steps_done`, `steps_total`.
- `StepView.is_current`.

Три чистые функции в `internal/projects/display_status.go`:

- `DeriveProjectDisplayStatus(projectStatus, steps) ProjectDisplayStatus`
  — терминальные project.status побеждают; иначе анализ шагов.
  waiting_action (есть waiting_client + owner=client) приоритетнее
  in_progress.
- `DeriveStageDisplayStatus(steps) (status, done, total)`.
- `DeriveCurrentStep(steps) *StepView`: приоритет waiting_client+client
  → in_progress → waiting_client (любой owner) → первый pending.

Применить в `service.GetClientProject`/`GetClientFunnel`: после сбора
стадий и шагов прогнать derive-функции, заполнить поля, пометить
`is_current=true` на выбранном шаге.

Минимум 10 тестов:

- not_started/in_progress/waiting_action детектируются;
- waiting_action приоритетнее in_progress;
- project.status=done/cancelled/on_hold/dispute → соответствующий
  DisplayStatus;
- stage 3/3 → completed; 1/3 → active; 0/3 без active → not_started;
  0/3 с in_progress → active.

## Frontend (Phases 2-5)

`entities/project/model/project.types.ts`: расширить типы.

`shared/lib/project-status.ts`:

- `PROJECT_STATUS_LABELS` (рус. строки)
- `PROJECT_STATUS_COLOR` (nz-tag color)
- `STAGE_STATUS_COLOR`
- `getStepBadge(status, owner): { label, color }`
- `OWNER_LABEL`

`shared/lib/step-descriptions.ts`: `Record<step_code, string>` для
сабтайтла в шапке детальной (payment → «Ожидаем оплату…», и т.д.).

UI-правки:

- **project-card**: бейдж из PROJECT_STATUS_LABELS/_COLOR, строка
  «Сейчас: {current_step_title}», прогресс-бар без «0%» при 0,
  жёлтый left-border 4px при waiting_action.
- **funnel-stage** (стадия): бейдж заменить на «N из M» (active) /
  «Готово» (completed) / скрыть (not_started). active — лёгкая
  подсветка, completed — opacity 0.6.
- **funnel-stage** (шаг): фон/border/бейдж по таблице (pending,
  in_progress team/system, waiting_client, done, rejected, skipped).
  is_current → glow + лейбл «Сейчас». Owner-лейбл: «Команда
  исполнителя» / «Ваше действие» / «Автоматически».
- **project-detail hero**: блок current-block с
  `PROJECT_STATUS_LABELS` бейджем + h2 current_step_title + p
  step-description. Прогресс-бар без «0%».
- Кнопки **«Принять» / «Запросить правки»** — внутри шага
  waiting_client+owner=client, не в шапке отдельно.

## Что НЕ делать

- Не переписывать структуру файлов.
- Не менять SQL-схему.
- Не вводить новые UI-киты.
- Не плодить эмодзи в продакшен-UI.
- Не перекрашивать проект целиком.

## DoD

Карточка на 77% показывает «В работе», не «Скоро начнётся». Стадия с
0/3 — без «3 в работе». Шаги визуально различимы по статусу.
Текущий шаг с лейблом «Сейчас». waiting_action карточка с жёлтым
бордером. При 0% — нет надписи «0%».
