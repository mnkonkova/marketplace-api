-- +goose Up
-- +goose StatementBegin

-- «Это ваш ролик?» — найденное на площадке, но ещё не привязанное.
--
-- Сейчас ссылку приносит креатор руками, и именно на этом теряются
-- площадки: ролик вышел, а в сервисе его нет, потому что человек забыл
-- вставить адрес. Сервис умеет найти ролик на аккаунте креатора сам —
-- не хватало места, куда положить находку до того, как человек скажет
-- «да, мой».
--
-- Хранится ИМЕННО НАХОДКА, без выкладки: к какой выкладке её
-- предложить — вопрос сегодняшнего дня (одну закрыли, другую
-- перенесли), и вычисляется он при чтении. Прибитый сюда
-- publication_id протух бы на первом же переносе даты.
CREATE TABLE IF NOT EXISTS publication_link_suggestions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform        TEXT NOT NULL,
    -- url — как отдала площадка, url_canonical — по чему сверяем.
    url             TEXT NOT NULL,
    url_canonical   TEXT NOT NULL,
    -- title/author_handle — чтобы человек узнал свой ролик, не открывая
    -- ссылку: «Нашли в TikTok @anya.kim от 16.09: „Раф без сахара“».
    title           TEXT NOT NULL DEFAULT '',
    author_handle   TEXT NOT NULL DEFAULT '',
    published_at    TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'linked', 'dismissed')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ,
    decided_by      UUID REFERENCES users(id)
);

-- Одна находка на проект: повторный обход аккаунта не должен плодить
-- одну и ту же карточку каждый день. Отказ «не мой» тоже хранится
-- строкой — иначе отвергнутое возвращалось бы на следующем обходе.
CREATE UNIQUE INDEX IF NOT EXISTS publication_link_suggestions_uniq
    ON publication_link_suggestions (project_id, url_canonical);

CREATE INDEX IF NOT EXISTS publication_link_suggestions_pending_idx
    ON publication_link_suggestions (creator_user_id, created_at DESC)
    WHERE status = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS publication_link_suggestions;

-- +goose StatementEnd
