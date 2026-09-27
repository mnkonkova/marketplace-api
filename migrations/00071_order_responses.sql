-- +goose Up
-- +goose StatementBegin

-- Отклик креатора на рассылку по заявке «под ключ».
--
-- Приглашение уходит ВСЕМ известным креаторам, и ответить можно тремя
-- способами — это дословно три значения mode, и разница между первыми
-- двумя не косметическая:
--
--   attach         — приложил файл в кабинете: файл уже наш, лежит в
--                    бакете под своим ключом;
--   upload         — прислал файл в бот, и мы кладём его ЕЩЁ И в
--                    кабинет, то есть в портфолио: человек присылал его
--                    нам, а не «в заявку», и искать его он пойдёт у
--                    себя;
--   from_portfolio — «отправить из моих»: ничего не грузим, показываем
--                    менеджеру уже загруженные ролики;
--   decline        — отказался. Строка нужна: «ответил нет» и «молчит»
--                    для менеджера разные вещи, а второе он видит как
--                    отсутствие строки.
--
-- Отклик — это НЕ согласие. Согласие подтверждает менеджер, когда
-- добавляет человека в состав проекта: заявка одна, а откликнуться на
-- неё могут двадцать.
CREATE TABLE order_candidate_responses (
    order_id        UUID NOT NULL,
    creator_user_id UUID NOT NULL,
    mode            TEXT NOT NULL
        CHECK (mode IN ('attach', 'upload', 'from_portfolio', 'decline')),
    -- file_url — объект в бакете под префиксом orders/.
    --
    -- ВАЖНО: колонка обязана быть в Repo.LoadReferencedMediaURLs
    -- (internal/profiles/repo.go), иначе SweepOrphanMedia снесёт живой
    -- файл через S3_ORPHAN_MIN_AGE; а префикс "orders/" обязан быть в
    -- prefixes там же (internal/profiles/cleanup.go), иначе удалённые
    -- отклики оставят мусор в бакете навсегда.
    file_url        TEXT,
    note            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, creator_user_id),
    -- Через order_candidates, а не напрямую в creator_orders: строка
    -- кандидата появляется у любого, кто откликнулся, и без неё отклик
    -- повис бы сам по себе — ни в очереди, ни в списке у менеджера.
    FOREIGN KEY (order_id, creator_user_id)
        REFERENCES order_candidates(order_id, creator_user_id) ON DELETE CASCADE
);

-- «Отправить из моих»: какие именно ролики креатор показал.
--
-- Отдельной таблицей, а не массивом id: удалённый элемент портфолио
-- обязан исчезнуть и из отклика, иначе менеджер откроет карточку и
-- упрётся в битую ссылку. Это и делает ON DELETE CASCADE.
CREATE TABLE order_response_portfolio_items (
    order_id        UUID NOT NULL,
    creator_user_id UUID NOT NULL,
    item_id         UUID NOT NULL REFERENCES portfolio_items(id) ON DELETE CASCADE,
    PRIMARY KEY (order_id, creator_user_id, item_id),
    FOREIGN KEY (order_id, creator_user_id)
        REFERENCES order_candidate_responses(order_id, creator_user_id) ON DELETE CASCADE
);

-- Список откликов у менеджера: «кто согласен» по одной заявке.
CREATE INDEX order_candidate_responses_order_idx
    ON order_candidate_responses(order_id, created_at DESC);

-- Договор в материалах проекта.
--
-- Отдельный вид рядом с doc/video/link (00032), а не материал с
-- названием «Договор»: договор отдаётся первым и выделяется в кабинете
-- креатора плашкой, а искать его подстрокой в названии — значит
-- однажды не найти. Это первое, что человек ищет, когда его добавили в
-- проект, и последнее, что он хочет искать глазами в списке из
-- тридцати референсов.
ALTER TABLE project_materials DROP CONSTRAINT project_materials_kind_check;
ALTER TABLE project_materials ADD CONSTRAINT project_materials_kind_check
    CHECK (kind IN ('doc', 'video', 'link', 'contract'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS order_response_portfolio_items;
DROP TABLE IF EXISTS order_candidate_responses;
UPDATE project_materials SET kind = 'doc' WHERE kind = 'contract';
ALTER TABLE project_materials DROP CONSTRAINT project_materials_kind_check;
ALTER TABLE project_materials ADD CONSTRAINT project_materials_kind_check
    CHECK (kind IN ('doc', 'video', 'link'));
-- +goose StatementEnd
