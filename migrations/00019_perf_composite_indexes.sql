-- +goose Up
-- Composite-индексы под горячие ORDER BY/WHERE-комбинации.
-- Профильные таблицы (specialist_profiles, client_profiles) — уже PK
-- по user_id, для JOIN'ов в LoadClientDisplayNames/LoadPartyContacts
-- этого достаточно. Outbox refreshGauges уже покрыт outbox_pending_idx
-- (00007) + outbox_dead_idx — не трогаем.

-- P15: reviews.ListByTarget — ORDER BY created_at DESC LIMIT/OFFSET.
-- Текущий reviews_target_idx (target_user_id) принуждает Sort на >100k
-- отзывов спеца. Composite убирает sort step.
CREATE INDEX IF NOT EXISTS reviews_target_created_idx
  ON reviews (target_user_id, created_at DESC);

-- P16-a: ListAssignedTo — менеджерский канбан/список своих проектов
-- с ORDER BY updated_at DESC. Без composite Sort срабатывает поверх
-- projects_assigned_idx — на менеджере с 1k+ проектов это в planning
-- time добавляет миллисекунды, в execution — sort 1k строк.
CREATE INDEX IF NOT EXISTS projects_assigned_updated_idx
  ON projects (assigned_to_user_id, updated_at DESC)
  WHERE assigned_to_user_id IS NOT NULL;

-- P16-b: ListBySpecialist — спец видит свои проекты, тот же паттерн.
-- Partial WHERE specialist_user_id IS NOT NULL — большая часть строк
-- (proposed-only) специалиста ещё не имеет, индекс мельче.
CREATE INDEX IF NOT EXISTS projects_specialist_updated_idx
  ON projects (specialist_user_id, updated_at DESC)
  WHERE specialist_user_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS projects_specialist_updated_idx;
DROP INDEX IF EXISTS projects_assigned_updated_idx;
DROP INDEX IF EXISTS reviews_target_created_idx;
