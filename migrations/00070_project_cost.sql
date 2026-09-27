-- +goose Up
-- +goose StatementBegin

-- Стоимость проекта, названная менеджером.
--
-- У проекта с креаторами сумма никогда не вводится целиком: она
-- складывается из начислений людям (creator_accruals.total), и СПВ
-- считается по ней — totals() делит сумму строк на их просмотры.
-- У проекта без креаторов складывать нечего: людей нет, начислений нет,
-- а вопрос «во сколько обошёлся просмотр» остаётся главным.
--
-- Поэтому сумму называет менеджер. Это такое же условие проекта, как
-- ставка за тысячу, и живёт оно там же — в снимке условий project_billing
-- (00037:45). Заводить под одно число вторую денежную модель со своей
-- таблицей, своей историей и своим экраном значило бы держать два ответа
-- на вопрос «сколько стоит проект» и однажды получить разные.
--
-- Ноль означает «не назвали»: СПВ в этом случае не показывается вовсе, а
-- не показывается нулём — «бесплатно» и «не знаем» разные вещи.
ALTER TABLE project_billing
    ADD COLUMN IF NOT EXISTS project_cost BIGINT NOT NULL DEFAULT 0;

ALTER TABLE project_billing
    ADD CONSTRAINT project_billing_cost_nonneg CHECK (project_cost >= 0);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE project_billing DROP CONSTRAINT IF EXISTS project_billing_cost_nonneg;
ALTER TABLE project_billing DROP COLUMN IF EXISTS project_cost;

-- +goose StatementEnd
