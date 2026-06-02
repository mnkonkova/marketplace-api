-- +goose Up
-- Заменяем users.role (enum: client|specialist|manager|admin) на пару флагов
-- is_manager + is_admin. Раньше role смешивал «маркетплейс-идентичность»
-- (что и так живёт в users.kind) с «CRM-правами». Из-за чего регистрация
-- спеца ломала шапку: kind=specialist, role=client → showSpecialistCabinet
-- падал в false. Теперь права ортогональны: kind отвечает за каталог,
-- флаги — за кабинеты CRM. role в API остаётся, но собирается на лету
-- (admin > manager > kind=specialist → specialist, иначе client).
ALTER TABLE users
    ADD COLUMN is_manager BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN is_admin   BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE users SET is_manager = TRUE WHERE role = 'manager';
UPDATE users SET is_admin   = TRUE WHERE role = 'admin';

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users DROP COLUMN role;

-- +goose Down
ALTER TABLE users
    ADD COLUMN role TEXT NOT NULL DEFAULT 'client';

UPDATE users SET role = 'admin'   WHERE is_admin;
UPDATE users SET role = 'manager' WHERE is_manager AND NOT is_admin;
UPDATE users SET role = 'specialist'
    WHERE NOT is_admin AND NOT is_manager AND kind IN ('specialist', 'both');

ALTER TABLE users
    ADD CONSTRAINT users_role_check
        CHECK (role IN ('client','specialist','manager','admin'));

ALTER TABLE users
    DROP COLUMN is_manager,
    DROP COLUMN is_admin;
