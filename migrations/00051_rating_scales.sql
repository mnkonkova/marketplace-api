-- +goose Up
-- +goose StatementBegin

-- Справочник порогов оценок: по каким числам ролик называется плохим,
-- средним, хорошим, отличным, хитом или виральным.
--
-- Версионируется ровно как прайс, чеклисты и воронки — четвёртый раз тем
-- же приёмом, а не новым. Причина та же: поправят порог задним числом —
-- и все прошлые периоды молча пересчитаются, а клиент видел другое.
-- Поэтому версия НЕ правится: выпускается следующая, прежняя остаётся
-- как была, и подытоженный период помнит, какой его оценивали.
--
-- Денежных правил здесь нет. Гарантия, потолок, ступени и ставки — это
-- тариф, он версионируется отдельно (terms_versions). Два места про одни
-- и те же деньги разъедутся, и разбираться, какое из них правда, будет
-- некогда.
CREATE TABLE rating_scales (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version      INT NOT NULL UNIQUE,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- PublishedBy — кто выпустил. NULL у первой версии: её ставит
    -- миграция, и приписывать её человеку было бы враньём.
    published_by UUID REFERENCES users(id) ON DELETE SET NULL,
    -- Note — зачем выпустили. Через год «почему пороги такие» отвечается
    -- только этим.
    note TEXT NOT NULL DEFAULT '',

    -- 1. Уровни ролика по сумме просмотров на пяти площадках.
    -- Шесть уровней — пять границ: ниже первой плохой, дальше средний,
    -- хороший, отличный, хит, от последней виральный.
    level_medium_from BIGINT NOT NULL,
    level_good_from   BIGINT NOT NULL,
    level_great_from  BIGINT NOT NULL,
    level_hit_from    BIGINT NOT NULL,
    level_viral_from  BIGINT NOT NULL,

    -- 2. Медиана типичного ролика: от неё считается «сколько роликов до
    -- следующей ступени», когда своей истории у креатора мало.
    typical_video_views BIGINT NOT NULL,

    -- 4. Норма долей: сколько роликов какого уровня считается нормальным
    -- распределением, в процентах.
    share_bad_pct    INT NOT NULL,
    share_medium_pct INT NOT NULL,
    share_good_pct   INT NOT NULL,
    share_great_pct  INT NOT NULL,

    -- 5. Параметры относительной оценки. Сами перцентили считаются по
    -- данным проекта и не настраиваются — настраиваются только рамки,
    -- в которых их считать.
    window_days       INT NOT NULL,
    min_mature_videos INT NOT NULL,
    mature_age_days   INT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT rating_scales_levels_ordered CHECK (
        level_medium_from < level_good_from
        AND level_good_from < level_great_from
        AND level_great_from < level_hit_from
        AND level_hit_from < level_viral_from
    ),
    CONSTRAINT rating_scales_typical_positive CHECK (typical_video_views > 0),
    CONSTRAINT rating_scales_shares_are_hundred CHECK (
        share_bad_pct + share_medium_pct + share_good_pct + share_great_pct = 100
    ),
    CONSTRAINT rating_scales_relative_positive CHECK (
        window_days > 0 AND min_mature_videos > 0 AND mature_age_days > 0
    )
);

-- 3. Уровни по площадкам: у каждой свои числа, и сравнивать ролик в VK с
-- роликом в YouTube по общей шкале бессмысленно — там разные порядки.
CREATE TABLE rating_scale_platforms (
    scale_id UUID NOT NULL REFERENCES rating_scales(id) ON DELETE CASCADE,
    platform TEXT NOT NULL,
    -- Три границы — четыре уровня: ниже первой слабо, дальше средне,
    -- хорошо, от последней отлично. Общая шкала подробнее, площадочная
    -- грубее: на площадке важно «тянет или нет», а не шесть оттенков.
    medium_from BIGINT NOT NULL,
    good_from   BIGINT NOT NULL,
    great_from  BIGINT NOT NULL,
    PRIMARY KEY (scale_id, platform),
    CONSTRAINT rating_scale_platforms_known CHECK (
        platform IN ('tiktok', 'instagram', 'youtube', 'vk', 'likee')
    ),
    CONSTRAINT rating_scale_platforms_ordered CHECK (
        medium_from < good_from AND good_from < great_from
    )
);

-- 6. Ориентиры рынка: сколько стоит тысяча просмотров у других.
--
-- У каждого числа СВОЙ источник и СВОЯ дата, и это не бюрократия: они
-- показываются клиенту и протухают. Без даты через год цифра начнёт
-- врать, и заметить это будет нечем.
CREATE TABLE rating_scale_market (
    scale_id UUID NOT NULL REFERENCES rating_scales(id) ON DELETE CASCADE,
    -- Key — что именно: bloggers | rsya_banners | rsya_video.
    key TEXT NOT NULL,
    -- Title — как показать человеку.
    title TEXT NOT NULL,
    -- PricePer1000 — цена за тысячу показов, копейки.
    price_per_1000 BIGINT NOT NULL,
    source      TEXT NOT NULL,
    measured_on DATE NOT NULL,
    PRIMARY KEY (scale_id, key),
    CONSTRAINT rating_scale_market_price_positive CHECK (price_per_1000 >= 0),
    CONSTRAINT rating_scale_market_has_source CHECK (source <> '')
);

-- Копия справочника у проекта. Ссылкой, а не переписыванием чисел:
-- версия неизменяема по построению, поэтому ссылка на неё — это и есть
-- снимок. Так же, как проект ссылается на версию чеклиста.
ALTER TABLE projects
    ADD COLUMN rating_scale_id UUID REFERENCES rating_scales(id);

-- Подытоженный период помнит, какой версией его оценивали.
--
-- Оценка — утверждение о прошлом. Период уже замораживает просмотры и
-- суммы; пороги — такая же его часть: поменяли шкалу — прошлое не должно
-- переписаться.
ALTER TABLE project_periods
    ADD COLUMN rating_scale_id UUID REFERENCES rating_scales(id);

-- Первая версия: числа из аналитики владельца продукта по 253 роликам за
-- июнь–сентябрь 2026. Заводим миграцией, а не руками на проде: без
-- действующей версии справочник пустой, и первый же запрос упрётся в
-- «нет данных» на ровном месте.
INSERT INTO rating_scales (
    version, note,
    level_medium_from, level_good_from, level_great_from, level_hit_from, level_viral_from,
    typical_video_views,
    share_bad_pct, share_medium_pct, share_good_pct, share_great_pct,
    window_days, min_mature_videos, mature_age_days
) VALUES (
    1, 'Первая версия: аналитика по 253 роликам за июнь–сентябрь 2026',
    2000, 6000, 15000, 50000, 300000,
    3000,
    30, 45, 15, 10,
    90, 100, 14
);

INSERT INTO rating_scale_platforms (scale_id, platform, medium_from, good_from, great_from)
SELECT s.id, v.platform, v.medium_from, v.good_from, v.great_from
FROM rating_scales s
CROSS JOIN (VALUES
    ('youtube',   550::bigint, 2800::bigint, 9000::bigint),
    ('instagram', 160,         1700,         3000),
    ('tiktok',    630,         930,          1800),
    ('likee',     350,         880,          1000),
    ('vk',        2,           16,           140)
) AS v(platform, medium_from, good_from, great_from)
WHERE s.version = 1;

INSERT INTO rating_scale_market (scale_id, key, title, price_per_1000, source, measured_on)
SELECT s.id, v.key, v.title, v.price, v.source, v.measured_on::date
FROM rating_scales s
CROSS JOIN (VALUES
    -- Числа и источники — из коммерческого предложения (сентябрь 2026).
    -- Не выдумывать: их показывают клиенту рядом с нашей ценой, и
    -- неверный ориентир не просто врёт, а врёт в нашу пользу.
    ('bloggers',     'Реклама у блогеров', 100000::bigint,
     'ориентир 1 ₽ за просмотр, коммерческое предложение', '2026-09-01'),
    ('rsya_banners', 'Баннеры РСЯ',         42500::bigint,
     'Ingate, середина диапазона 250–600 ₽ за 1000 показов', '2026-04-07'),
    ('rsya_video',   'Видео РСЯ',           32500::bigint,
     'Ingate, середина диапазона 150–500 ₽ за 1000 показов', '2026-04-07')
) AS v(key, title, price, source, measured_on)
WHERE s.version = 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE project_periods DROP COLUMN IF EXISTS rating_scale_id;
ALTER TABLE projects DROP COLUMN IF EXISTS rating_scale_id;
DROP TABLE IF EXISTS rating_scale_market;
DROP TABLE IF EXISTS rating_scale_platforms;
DROP TABLE IF EXISTS rating_scales;

-- +goose StatementEnd
