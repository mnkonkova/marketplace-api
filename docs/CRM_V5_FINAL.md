# CRM v5 — финализация (Ф13)

Все 13 фаз реализованы. Этот документ — чек-лист по DoD + что отложено
+ инструкция «как поднять локально и пройти приёмочные сценарии».

## Реализовано

### Backend (Ф1–Ф8)
- [x] Миграция 00010_crm.sql: users.role + is_approved, productions,
      specialist_profiles.production_id/is_freelance, pipelines/stages/steps,
      projects + project_stages/steps, project_step_events, project_comments,
      client_invites, reviews.project_id.
- [x] Домен productions (CRUD + публичный список).
- [x] Домен pipelines (CRUD + полное дерево + reorder).
- [x] Домен projects (StartProject со снэпшотом, display_status,
      клиентский/менеджерский/админский API, комментарии, события).
- [x] Стейт-машина шагов и стадий, advance_stage с защитой от пропуска
      клиентского апрува.
- [x] Review-шаг авто-skip через 7 дней (worker ticker).
- [x] Auth.Register с role (manager → is_approved=false).
- [x] Admin: approve/revoke менеджера, manual project, magic-link инвайты.
- [x] outbox → n8n webhook (CRM-события project.*).

### Frontend (Ф9–Ф12)
- [x] Клиентский раздел /me/projects (список + детальная воронка с
      apply/revision/review).
- [x] Специалистский кабинет /me/specialist (выбор продакшена + назначенные).
- [x] Менеджерский кабинет /manager (inbox + канбан CDK + детальная страница
      с лентой активности и комментариями).
- [x] Админский кабинет /admin (productions/managers/pipelines-editor с
      CDK reorder/projects/board).
- [x] AppHeader: пункты по роли (Мои проекты / Продакшен / Менеджер / Админ).
- [x] AuthSessionStore с role/is_approved + fetchMe().
- [x] Magic-link /auth/invite?token=.

### n8n (Ф8 + Ф13)
- [x] WebhookDispatcher в worker (Bearer-auth, JSON payload с event_id).
- [x] ops/n8n/README.md — описание формата.
- [x] ops/n8n/workflows/project-client-notification.json — заготовка.
- [x] ops/n8n/workflows/manager-disputed.json — заготовка Telegram.

## Отложено

- `POST /admin/projects/from_recipient/{id}` и `from_lead/{id}/bulk` —
  требуют интеграции с leads. Реализуются простой обёрткой над
  `StartProject` после выборки lead/recipient. Не критично для MVP.
- Frontend guards (client/manager/admin/specialist) — header сейчас
  скрывает чужие пункты, но прямой ввод URL не блокируется. Добавить
  `CanActivate` гарды по `auth.role()`.
- Интеграционные тесты с реальной БД (advance_stage, auto-skip review,
  redeem_invite). Текущие тесты — unit на чистые функции/валидаторы.
- TipTap-редактор для комментариев. Backend поддерживает body_format
  (`plain` | `html` | `tiptap_json`); фронт сейчас всегда шлёт plain.
  Точка расширения помечена `TODO` в шаблоне.

## Локальный запуск (для приёмки)

```sh
# 1. БД и сервисы
make up                # postgres + opensearch + redis + (по желанию) n8n
make migrate-up        # накатить все миграции, включая 00010

# 2. API + worker
make run               # API на :8080
make run-worker        # outbox-воркер с review-tick

# 3. Frontend
cd ../marketplace-web/web
npm run start          # на :5173
```

## Приёмочные сценарии (вручную)

1. **Админ создаёт пайплайн** через `/admin/pipelines` → редактор →
   стадии «бриф → оплата → монтаж → апрув клиента → отзыв» с
   `duration_days`, `owner`, флагом `is_review` на последнем шаге.
2. **Регистрация менеджера**: POST `/auth/register` с `role=manager` →
   `is_approved=false`. Логин в `/manager` → 403 `forbidden_unapproved`.
3. **Админ аппрувит** через `/admin/managers` → 204. Менеджер заходит
   в `/manager` → видит inbox.
4. **Создание проекта**: админ через `/admin/projects` POST с
   `client_user_id + pipeline_id + title` → проект появляется в
   inbox менеджера.
5. **Claim**: менеджер берёт проект → попадает в `/manager/board` (канбан).
6. **Канбан drag**: тащит карточку в следующую стадию → `advance_stage` →
   карточка переезжает. Если в текущей стадии есть незавершённый
   client-шаг → 409 `stage_blocked` → карточка откатывается.
7. **Клиент**: видит проект в `/me/projects`, апрувит свой шаг, видит
   обновлённый прогресс.
8. **Цикл правок**: клиент жмёт «Запросить правки» → шаг rejected,
   предыдущий team-шаг возвращается в in_progress. Лимит — `dispute`.
9. **Финальный отзыв**: клиент попадает на review-шаг (waiting_client +
   is_review=true) → жмёт «Оставить отзыв» → шаг done. Если 7 дней
   проигнорировал — worker auto-skip-нет, проект закрыт.
10. **n8n**: webhook получает все события, можно проверить в Executions
    dashboard.

## DoD

См. `docs/CRM_V5_BRIEF.md §10` — чек-лист.
