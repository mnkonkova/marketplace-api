-- +goose Up
-- +goose StatementBegin

-- Добавляем категорию «Режиссёр» в Производство — отдельная роль от
-- video_director (тот про монтаж). Этот — про полный цикл production:
-- концепция, кастинг, режиссура на площадке, контроль до релиза.
INSERT INTO specialty_categories (code, title, description, type, sort_order) VALUES
    ('director', 'Режиссёр', 'Концепция, кастинг, режиссура на площадке, ведение проекта от идеи до сдачи', 'Производство', 15);

-- Новые навыки специфичные для режиссуры (не дублируем существующие
-- editing-direction / storytelling — те остаются у video_director,
-- но также мапятся на director ниже как кросс-навыки).
INSERT INTO skills (slug, title, kind) VALUES
    ('film-directing',       'Кинорежиссура',                     'skill'),
    ('commercial-directing', 'Рекламная режиссура',               'skill'),
    ('casting',              'Кастинг и работа с актёрами',       'skill'),
    ('production-planning',  'Препродакшн / план съёмок',         'skill'),
    ('on-set-directing',     'Работа на площадке',                'skill'),
    ('mood-board',           'Мудборды / референсы',              'skill');

-- Маппинг skill_categories для director. Часть навыков пересекается с
-- video_director (сторителлинг, режиссура монтажа) и scriptwriter
-- (сценарии) — это нормально, режиссёр должен понимать смежные слои.
INSERT INTO skill_categories (skill_id, category_code)
SELECT s.id, v.code
FROM skills s
JOIN (VALUES
    ('film-directing',       'director'),
    ('commercial-directing', 'director'),
    ('casting',              'director'),
    ('production-planning',  'director'),
    ('on-set-directing',     'director'),
    ('mood-board',           'director'),
    ('storytelling',         'director'),
    ('editing-direction',    'director'),
    ('scriptwriting',        'director'),
    ('videography',          'director'),
    ('premiere',             'director'),
    ('davinci',              'director'),
    ('final-cut',            'director')
) AS v(slug, code) ON v.slug = s.slug;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM skill_categories WHERE category_code = 'director';
DELETE FROM skills WHERE slug IN (
    'film-directing', 'commercial-directing', 'casting',
    'production-planning', 'on-set-directing', 'mood-board'
);
DELETE FROM specialty_categories WHERE code = 'director';

-- +goose StatementEnd
