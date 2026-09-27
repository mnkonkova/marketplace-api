-- +goose Up
-- +goose StatementBegin

-- Фикс считается ЗА РОЛИК, а не за период.
--
-- Прежняя модель платила фиксированную сумму за месяц работы независимо
-- от того, сколько роликов в нём вышло. Это било в обе стороны: месяц с
-- пятью выкладками стоил столько же, сколько месяц с тридцатью, и
-- договориться о другом объёме можно было только новой ценой периода.
--
-- Теперь фикс — цена одного ролика: клиент платит fee_per_video за
-- каждую вышедшую выкладку, креатор получает creator_fee_per_video.
-- Ступени за просмотры остаются как были — они про рост, а не про
-- объём работы.
--
-- NULL означает «фикс за ролик не задан» и включает прежнее правило:
-- выключенная новая механика не должна молча обнулять счёт идущим
-- проектам.
ALTER TABLE project_billing
    ADD COLUMN IF NOT EXISTS fee_per_video BIGINT,
    ADD COLUMN IF NOT EXISTS creator_fee_per_video BIGINT;

ALTER TABLE terms_versions
    ADD COLUMN IF NOT EXISTS fee_per_video BIGINT,
    ADD COLUMN IF NOT EXISTS creator_fee_per_video BIGINT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE terms_versions
    DROP COLUMN IF EXISTS creator_fee_per_video,
    DROP COLUMN IF EXISTS fee_per_video;
ALTER TABLE project_billing
    DROP COLUMN IF EXISTS creator_fee_per_video,
    DROP COLUMN IF EXISTS fee_per_video;
-- +goose StatementEnd
