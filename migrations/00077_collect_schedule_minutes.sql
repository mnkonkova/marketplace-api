-- +goose Up
-- +goose StatementBegin

-- Частый обход свежего ролика.
--
-- Было: один ролик обходится не чаще раза в КАЛЕНДАРНЫЙ день, шаг
-- растёт по возрасту 1→2→4→8 дней. Для архива это правильно, но для
-- вышедшего час назад ролика означало вот что: в кабинете ноль
-- просмотров у ролика, который на площадке уже набрал тысячу, и
-- обновить это нельзя никак — ни кнопкой, ни настройкой. Ноль при этом
-- неотличим от «никто не смотрит», то есть врёт сильнее, чем пустота.
--
-- Стало: в первые сутки ролик обходится часто и с затухающим шагом
-- (минута, пять, десять, полчаса, час, шесть часов), а дальше падает в
-- ту же ленивую лестницу 1→2→4→8 дней. Девять обходов на свежий ролик
-- против одного — расход заметный, но конечный и затухающий; «всегда
-- раз в десять минут» стоило бы 144 похода в сутки на каждую живую
-- ссылку, при потолке воркера 4800 и кредите поставщика за каждый.
--
-- Суточное правило из выборки при этом снимается: пока оно стояло,
-- любой шаг меньше суток был невыразим в принципе.

-- Расписание в интервалах, а не в днях: «десять минут» в колонке дней
-- не выражается никак, а округление до единицы вернуло бы сутки.
ALTER TABLE publication_links
    ADD COLUMN IF NOT EXISTS collect_every INTERVAL NOT NULL DEFAULT interval '1 day';

UPDATE publication_links
SET collect_every = make_interval(days => GREATEST(collect_interval_days, 1));

ALTER TABLE publication_links DROP CONSTRAINT IF EXISTS links_interval_positive;
ALTER TABLE publication_links DROP COLUMN IF EXISTS collect_interval_days;

ALTER TABLE publication_links
    ADD CONSTRAINT links_collect_every_positive CHECK (collect_every > interval '0');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE publication_links
    ADD COLUMN IF NOT EXISTS collect_interval_days INT NOT NULL DEFAULT 1;

-- Обратно в дни с округлением вверх: сутки чаще, чем было, безопаснее
-- молчаливого нуля, на который ляжет CHECK.
UPDATE publication_links
SET collect_interval_days = GREATEST(CEIL(EXTRACT(EPOCH FROM collect_every) / 86400)::int, 1);

ALTER TABLE publication_links DROP CONSTRAINT IF EXISTS links_collect_every_positive;
ALTER TABLE publication_links DROP COLUMN IF EXISTS collect_every;
ALTER TABLE publication_links
    ADD CONSTRAINT links_interval_positive CHECK (collect_interval_days > 0);

-- +goose StatementEnd
