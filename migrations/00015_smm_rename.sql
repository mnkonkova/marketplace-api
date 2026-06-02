-- +goose Up
-- Переименовываем title категории smm с кириллического «СММ» на латинское «SMM».
-- В 00001_init.sql сидлайн уже обновлён — это fallback для баз, где
-- миграция 00001 уже применена со старым названием.
-- Code (smm) не трогаем — он FK для specialist_categories и используется
-- в специалистских профилях.
UPDATE specialty_categories SET title = 'SMM' WHERE code = 'smm' AND title = 'СММ';

-- +goose Down
UPDATE specialty_categories SET title = 'СММ' WHERE code = 'smm' AND title = 'SMM';
