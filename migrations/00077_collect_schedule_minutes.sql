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
-- Стало: расписаний ДВА, и это разные правила.
--
-- Фоновое (collectSchedule) осталось ленивым: 1→2→4→8 дней по возрасту
-- ролика. Оно про расход и в этой миграции не меняется.
--
-- Частое живёт в RefreshEvery и работает ТОЛЬКО по заходу в карточку:
-- минута, пять, десять, полчаса в первые полтора часа, дальше раз в час
-- до конца вторых суток и раз в шесть часов на всём остальном. Платится
-- оно лишь пока на проект смотрят; «всегда раз в десять минут» стоило бы
-- 144 похода в сутки на каждую живую ссылку, при потолке воркера 4800.
--
-- Колонка интервала всё равно нужна в минутах: шаг в десять минут в
-- колонке дней не выражается никак.
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
