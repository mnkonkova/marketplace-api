-- +goose Up
-- Контакты специалиста для прямой связи. Видимы только менеджеру после
-- создания заявки (POST /leads → ответ содержит контакты выбранных спецов);
-- никогда не уезжают в feed/search/публичный профиль.
ALTER TABLE specialist_profiles
    ADD COLUMN contact_email TEXT,
    ADD COLUMN contact_phone TEXT;

-- +goose Down
ALTER TABLE specialist_profiles
    DROP COLUMN contact_phone,
    DROP COLUMN contact_email;
