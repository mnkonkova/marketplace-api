-- +goose Up
-- +goose StatementBegin

-- Meta (Instagram/Facebook) признана в РФ экстремистской организацией;
-- по требованию законодательства все упоминания должны сопровождаться
-- сноской. Помечаем skill title через звёздочку; полный текст сноски
-- рендерится в site-footer'е (см. support-footer.component).
UPDATE skills SET title = 'Reels*' WHERE slug = 'reels' AND title = 'Instagram Reels';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

UPDATE skills SET title = 'Instagram Reels' WHERE slug = 'reels' AND title = 'Reels*';

-- +goose StatementEnd
