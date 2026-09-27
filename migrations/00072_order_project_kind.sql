-- +goose Up
-- +goose StatementBegin

-- Какой проект создаёт эта заявка.
--
-- Колонкой в заказе, а не выводом из содержимого: «нет отмеченных
-- креаторов» — это и «вторая ветка воронки», и «заказ, где никого не
-- отметили», и различать их догадкой значит однажды завести проект не
-- того вида. Обнаружится это не сразу: у проекта просто не окажется
-- состава, чеклиста и кабинета креатора — а выглядеть будет как
-- поломка кабинета.
--
-- TEXT + CHECK, а не общий ENUM project_kind: заказ порождает не любой
-- вид проекта, а ровно один из двух. Тянуть сюда production_turnkey и
-- general, которых из заказа не бывает, значит молча их разрешить.
--
-- DEFAULT — первая ветка: все заказы до этой колонки созданы ею.
ALTER TABLE creator_orders
    ADD COLUMN project_kind TEXT NOT NULL DEFAULT 'creators_turnkey'
        CHECK (project_kind IN ('creators_turnkey', 'brand_turnkey'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE creator_orders DROP COLUMN IF EXISTS project_kind;
-- +goose StatementEnd
