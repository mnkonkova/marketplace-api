-- +goose Up
-- Менеджер/админ могут заводить проект для клиента БЕЗ аккаунта на платформе.
-- В таком случае client_user_id=NULL, а контактные данные хранятся прямо
-- на проекте (как у анонимного lead'a). Если клиент потом зарегистрируется,
-- менеджер может вручную привязать аккаунт через UPDATE.
--
-- До этой миграции client_user_id был NOT NULL — единственный способ
-- завести проект был через клиента-с-аккаунтом или StartFromLead для
-- авторизованного клиента.

ALTER TABLE projects
    ALTER COLUMN client_user_id DROP NOT NULL;

ALTER TABLE projects
    ADD COLUMN client_name    TEXT,
    ADD COLUMN client_contact TEXT;

-- Инвариант: должен быть либо client_user_id, либо client_name+client_contact.
-- Без этого можно завести «проект без клиента вообще», что бессмысленно.
ALTER TABLE projects
    ADD CONSTRAINT projects_client_present CHECK (
        client_user_id IS NOT NULL
        OR (client_name IS NOT NULL AND client_name <> ''
            AND client_contact IS NOT NULL AND client_contact <> '')
    );

-- +goose Down
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_client_present;
ALTER TABLE projects DROP COLUMN IF EXISTS client_contact;
ALTER TABLE projects DROP COLUMN IF EXISTS client_name;
-- Не возвращаем NOT NULL: down может найти строки без client_user_id,
-- которые мы успели создать. Если нужно — сначала ручной UPDATE.
