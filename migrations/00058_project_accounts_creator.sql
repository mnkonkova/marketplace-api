-- +goose Up
-- +goose StatementBegin

-- Чей это аккаунт.
--
-- Доступы заводились как «аккаунты бренда» — общий список на проект. На
-- деле ролики выходят С АККАУНТОВ КРЕАТОРОВ: у каждого свой TikTok, свой
-- Reels, свой VK, и именно их заказчик спрашивает после запуска. Общим
-- списком это читается как чужая связка ключей: видно пять строк и
-- непонятно, кому какая принадлежит и с кого спрашивать, если ссылка
-- перестала отвечать.
--
-- Поле NULLABLE намеренно: бывают и настоящие брендовые доступы —
-- почта, рекламный кабинет, аккаунт самого бренда. У них владельца
-- среди креаторов нет, и выдумывать его нельзя.
--
-- ON DELETE SET NULL, а не CASCADE: креатора могут убрать из состава
-- или деактивировать, а доступ остаётся — по нему всё ещё выходят
-- ролики, и стирать его вместе с человеком значит потерять то, чего
-- заказчик и ждёт от этого списка.
ALTER TABLE project_accounts
    ADD COLUMN creator_user_id UUID REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX project_accounts_creator_idx
    ON project_accounts(project_id, creator_user_id);

-- Один аккаунт на площадку у человека в проекте: два «его TikTok» в
-- списке — это не два аккаунта, а один, заведённый дважды. Частичный
-- индекс, потому что у брендовых доступов владельца нет и их на одну
-- площадку может быть несколько.
CREATE UNIQUE INDEX project_accounts_creator_platform_uniq
    ON project_accounts(project_id, creator_user_id, platform)
    WHERE creator_user_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS project_accounts_creator_platform_uniq;
DROP INDEX IF EXISTS project_accounts_creator_idx;
ALTER TABLE project_accounts DROP COLUMN IF EXISTS creator_user_id;
-- +goose StatementEnd
