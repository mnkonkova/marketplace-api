-- +goose Up
-- Бэкфилл contact_email/contact_phone из users.email/users.phone для
-- специалистов, которые зарегистрировались до 00004. Перезаписываем только
-- пустые слоты — если юзер уже сам что-то вписал в /me, не трогаем.
UPDATE specialist_profiles sp
SET
    contact_email = COALESCE(sp.contact_email, u.email::text),
    contact_phone = COALESCE(sp.contact_phone, u.phone)
FROM users u
WHERE u.id = sp.user_id
  AND (sp.contact_email IS NULL OR sp.contact_phone IS NULL);

-- +goose Down
-- Откат сделать корректно нельзя: после бэкфилла нельзя различить
-- сохранённые auth-данные и руками введённые контакты. No-op.
SELECT 1;
