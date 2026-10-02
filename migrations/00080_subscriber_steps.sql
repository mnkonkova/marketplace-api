-- +goose Up
-- +goose StatementBegin

-- Лесенка по подписчикам.
--
-- Ставка за подписчика осталась, но одной её мало: у подписчиков та же
-- природа, что у просмотров — «набрал больше, стоит дороже», — и
-- поштучная цена этого не выражает. Владелец просил два режима, как у
-- просмотров: за одного или ступенями.
--
-- ВАЖНО, чем это ОТЛИЧАЕТСЯ от лесенки просмотров. Та задаёт ЦЕНУ
-- ПЕРИОДА: взял порог — период стоит столько, и фикс она собой
-- заменяет. Лесенка подписчиков задаёт ДОПЛАТУ сверх цены периода —
-- иначе в тарифе оказались бы две цены одного периода, спорящие друг с
-- другом, и вопрос «по чему считать» решался бы порядком строк.
--
-- Храним в той же таблице с признаком вида: правило «ступень — это
-- порог и две цены» одно на оба случая, и второй такой же таблицей оно
-- разъехалось бы на первой же правке (см. комментарий в 00054).
ALTER TABLE terms_steps
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'views'
        CHECK (kind IN ('views', 'subscribers'));

-- Колонка from_views теперь значит «порог»: просмотры у kind='views',
-- подписчики у kind='subscribers». Имя оставлено прежним намеренно —
-- оно же уезжает в JSON тарифа и в черновики прайса, открытые прямо
-- сейчас в чужих браузерах.
COMMENT ON COLUMN terms_steps.from_views IS
    'Порог ступени: просмотры при kind=views, подписчики при kind=subscribers';

-- Уникальность порога — внутри своего вида: «от 1000 просмотров» и
-- «от 1000 подписчиков» это разные ступени, и запрещать вторую нельзя.
DROP INDEX IF EXISTS terms_steps_version_from_idx;
DROP INDEX IF EXISTS terms_steps_project_from_idx;
CREATE UNIQUE INDEX terms_steps_version_from_idx
    ON terms_steps (terms_version_id, kind, from_views) WHERE terms_version_id IS NOT NULL;
CREATE UNIQUE INDEX terms_steps_project_from_idx
    ON terms_steps (project_id, kind, from_views) WHERE project_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM terms_steps WHERE kind = 'subscribers';
DROP INDEX IF EXISTS terms_steps_version_from_idx;
DROP INDEX IF EXISTS terms_steps_project_from_idx;
CREATE UNIQUE INDEX terms_steps_version_from_idx
    ON terms_steps (terms_version_id, from_views) WHERE terms_version_id IS NOT NULL;
CREATE UNIQUE INDEX terms_steps_project_from_idx
    ON terms_steps (project_id, from_views) WHERE project_id IS NOT NULL;
ALTER TABLE terms_steps DROP COLUMN IF EXISTS kind;

-- +goose StatementEnd
