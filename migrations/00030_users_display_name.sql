-- Имя пользователя на уровне аккаунта.
--
-- Раньше display_name жило только в specialist_profiles, а профиль создаётся
-- лишь специалистам. Заказчик указывал имя при регистрации, и оно молча
-- терялось: в кабинете он видел пустоту, а менеджер в заявке — только email.
--
-- Специалистам колонка не мешает: у них имя по-прежнему редактируется в
-- профиле, здесь остаётся значение с регистрации.

-- +goose Up
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

-- Существующим специалистам переносим имя из профиля, чтобы колонка не
-- осталась пустой у тех, кто зарегистрировался до этой миграции.
UPDATE users u
SET display_name = p.display_name
FROM specialist_profiles p
WHERE p.user_id = u.id AND u.display_name = '';

-- +goose Down
ALTER TABLE users DROP COLUMN display_name;
