-- +goose Up
-- +goose StatementBegin

-- Журнал админских действий: кто, что сделал, над чем и когда.
--
-- До сих пор это писалось только в slog (см. admin.auditLog): годится,
-- чтобы grep'нуть «кто кого заблокировал в среду», но не годится, чтобы
-- показать историю в карточке человека — логи ротируются, и запрос по
-- ним из приложения не сделать.
--
-- Запись идёт в ТОЙ ЖЕ транзакции, что и само действие (см.
-- internal/audit). Отдельным подключением журнал врал бы при откате:
-- роль не снялась, а в истории «снял роль» осталось.
CREATE TABLE admin_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    -- ON DELETE SET NULL: запись о действии переживает удаление того, кто
    -- его совершил. Иначе удаление сотрудника стирало бы и историю его
    -- решений — ровно то, ради чего журнал заводят.
    actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    action        TEXT NOT NULL,
    -- object_type/object_id — над чем действие. id текстом и без внешнего
    -- ключа намеренно: объекты разных таблиц, а запись должна пережить
    -- удаление объекта.
    object_type   TEXT NOT NULL,
    object_id     TEXT NOT NULL DEFAULT '',
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Лента журнала — «последние сверху», это её единственный порядок.
CREATE INDEX admin_audit_log_created_idx ON admin_audit_log (created_at DESC);

-- «Что делал этот сотрудник» и «что делали с этим объектом» — два
-- вопроса, ради которых в журнал и ходят из карточки.
CREATE INDEX admin_audit_log_actor_idx
    ON admin_audit_log (actor_user_id, created_at DESC);
CREATE INDEX admin_audit_log_object_idx
    ON admin_audit_log (object_type, object_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS admin_audit_log;

-- +goose StatementEnd
