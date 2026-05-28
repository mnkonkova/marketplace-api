-- +goose Up
-- +goose StatementBegin

-- Расширяем CHECK на kind: добавляем 'skill' для не-инструментальных навыков
-- (видеомонтаж, сторителлинг, ретушь, SEO и т.п.). Старые kind 'tool',
-- 'platform', 'genre' оставляем неизменными — миграции 00001 уже на проде.
ALTER TABLE skills DROP CONSTRAINT skills_kind_check;
ALTER TABLE skills ADD CONSTRAINT skills_kind_check
    CHECK (kind IN ('tool', 'platform', 'genre', 'skill'));

-- Расширяем словарь навыков по канону «Приложения B» из тикета.
-- Один канонический slug на сущность; кросс-навыки (photoshop, after-effects,
-- premiere, copywriting) намеренно используются в нескольких категориях —
-- через таблицу skill_categories ниже, slug не дублируем.
INSERT INTO skills (slug, title, kind) VALUES
    -- editor (skills + tools)
    ('video-editing',        'Видеомонтаж',                       'skill'),
    ('color-grading',        'Цветокоррекция',                    'skill'),
    ('audio-editing',        'Обработка звука',                   'skill'),
    ('editing-theory',       'Теория монтажа',                    'skill'),
    ('audition',             'Adobe Audition',                    'tool'),
    ('videoleap',            'Videoleap',                         'tool'),
    -- video_director
    ('storytelling',         'Сторителлинг',                      'skill'),
    ('editing-direction',    'Режиссура монтажа',                 'skill'),
    -- motion
    ('motion-design',        'Моушн-дизайн',                      'skill'),
    ('cinema-4d',            'Cinema 4D',                         'tool'),
    -- designer (skills)
    ('graphic-design',       'Графический дизайн',                'skill'),
    ('illustration',         'Иллюстрирование',                   'skill'),
    ('vector-graphics',      'Векторная графика',                 'skill'),
    ('typography',           'Типографика',                       'skill'),
    ('branding',             'Разработка фирменного стиля',       'skill'),
    ('design-concept',       'Разработка дизайн-концепции',       'skill'),
    ('web-design',           'Веб-дизайн',                        'skill'),
    ('print-design',         'Полиграфический дизайн',            'skill'),
    ('packaging-design',     'Дизайн упаковки',                   'skill'),
    ('outdoor-design',       'Дизайн наружной рекламы',           'skill'),
    ('email-design',         'Дизайн рассылок',                   'skill'),
    ('retouching',           'Ретушь',                            'skill'),
    -- designer (tools)
    ('illustrator',          'Adobe Illustrator',                 'tool'),
    ('indesign',             'Adobe InDesign',                    'tool'),
    ('coreldraw',            'CorelDRAW',                         'tool'),
    ('adobe-xd',             'Adobe XD',                          'tool'),
    ('sketch',               'Sketch',                            'tool'),
    -- videographer
    ('videography',          'Видеосъёмка',                       'skill'),
    -- photographer
    ('photography',          'Фотография',                        'skill'),
    ('product-photography',  'Предметная фотосъёмка',             'skill'),
    ('photo-retouching',     'Фотомонтаж / ретушь',               'skill'),
    -- scriptwriter
    ('scriptwriting',        'Написание сценариев',               'skill'),
    ('copywriting',          'Копирайтинг',                       'skill'),
    -- smm
    ('smm-strategy',         'SMM-стратегия',                     'skill'),
    ('content-planning',     'Разработка контент-плана',          'skill'),
    ('content-marketing',    'Контент-маркетинг',                 'skill'),
    ('stories-making',       'Сторисмейкинг',                     'skill'),
    ('influencer-marketing', 'Инфлюенс-маркетинг',                'skill'),
    ('messenger-marketing',  'Мессенджер-маркетинг',              'skill'),
    -- ads_seo (skills)
    ('targeted-ads',         'Таргетированная реклама',           'skill'),
    ('contextual-ads',       'Контекстная реклама',               'skill'),
    ('retargeting',          'Ретаргетинг',                       'skill'),
    ('lead-generation',      'Лидогенерация',                     'skill'),
    ('campaign-planning',    'Планирование рекламных кампаний',   'skill'),
    ('seo',                  'SEO',                               'skill'),
    -- ads_seo (tools)
    ('yandex-direct',        'Яндекс.Директ',                     'tool'),
    ('google-ads',           'Google Ads',                        'tool'),
    ('yandex-metrica',       'Яндекс.Метрика',                    'tool'),
    ('google-analytics',     'Google Analytics',                  'tool'),
    -- ai_creator
    ('midjourney',           'Midjourney',                        'tool'),
    ('stable-diffusion',     'Stable Diffusion',                  'tool'),
    ('runway',               'Runway',                            'tool'),
    ('sora',                 'Sora',                              'tool'),
    ('kling',                'Kling',                             'tool'),
    ('suno',                 'Suno',                              'tool'),
    ('prompt-engineering',   'Промптинг',                         'skill'),
    -- actor
    ('acting',               'Актёрское мастерство',              'skill'),
    ('voiceover',            'Озвучка',                           'skill')
ON CONFLICT (slug) DO NOTHING;

-- Связь skill <-> category: какие навыки и инструменты система предлагает
-- выбрать специалисту, когда он отметил конкретную категорию. Many-to-many,
-- потому что один slug (photoshop, after-effects, copywriting, premiere)
-- встречается у нескольких категорий — это намеренно. Платформы
-- (reels/tiktok/youtube/...) сюда НЕ кладём — это отдельный фасет, который
-- показывается всегда поверх категории.
CREATE TABLE skill_categories (
    skill_id      UUID NOT NULL REFERENCES skills(id)                    ON DELETE CASCADE,
    category_code TEXT NOT NULL REFERENCES specialty_categories(code)    ON DELETE CASCADE,
    PRIMARY KEY (skill_id, category_code)
);
CREATE INDEX skill_categories_category_idx ON skill_categories(category_code);

-- Наполнение skill_categories по канону «Приложения B». JOIN по slug —
-- чтобы не таскать сгенерированные UUID и чтобы повтор seed'а / down+up был
-- идемпотентным.
INSERT INTO skill_categories (skill_id, category_code)
SELECT s.id, v.code
FROM skills s
JOIN (VALUES
    -- editor
    ('video-editing',        'editor'),
    ('color-grading',        'editor'),
    ('audio-editing',        'editor'),
    ('editing-theory',       'editor'),
    ('premiere',             'editor'),
    ('after-effects',        'editor'),
    ('davinci',              'editor'),
    ('final-cut',            'editor'),
    ('capcut',               'editor'),
    ('audition',             'editor'),
    ('videoleap',            'editor'),
    -- video_director
    ('storytelling',         'video_director'),
    ('editing-direction',    'video_director'),
    ('premiere',             'video_director'),
    ('davinci',              'video_director'),
    ('after-effects',        'video_director'),
    -- motion
    ('motion-design',        'motion'),
    ('after-effects',        'motion'),
    ('cinema-4d',            'motion'),
    -- designer
    ('graphic-design',       'designer'),
    ('illustration',         'designer'),
    ('vector-graphics',      'designer'),
    ('typography',           'designer'),
    ('branding',             'designer'),
    ('design-concept',       'designer'),
    ('web-design',           'designer'),
    ('print-design',         'designer'),
    ('packaging-design',     'designer'),
    ('outdoor-design',       'designer'),
    ('email-design',         'designer'),
    ('retouching',           'designer'),
    ('photoshop',            'designer'),
    ('illustrator',          'designer'),
    ('indesign',             'designer'),
    ('coreldraw',            'designer'),
    ('figma',                'designer'),
    ('adobe-xd',             'designer'),
    ('sketch',               'designer'),
    -- videographer
    ('videography',          'videographer'),
    ('premiere',             'videographer'),
    -- photographer
    ('photography',          'photographer'),
    ('product-photography',  'photographer'),
    ('photo-retouching',     'photographer'),
    ('photoshop',            'photographer'),
    -- scriptwriter
    ('scriptwriting',        'scriptwriter'),
    ('copywriting',          'scriptwriter'),
    -- smm
    ('smm-strategy',         'smm'),
    ('content-planning',     'smm'),
    ('content-marketing',    'smm'),
    ('copywriting',          'smm'),
    ('stories-making',       'smm'),
    ('influencer-marketing', 'smm'),
    ('messenger-marketing',  'smm'),
    -- ugc — кросс-инструмент для съёмки и быстрого монтажа на телефоне
    ('capcut',               'ugc'),
    -- ads_seo
    ('targeted-ads',         'ads_seo'),
    ('contextual-ads',       'ads_seo'),
    ('retargeting',          'ads_seo'),
    ('lead-generation',      'ads_seo'),
    ('campaign-planning',    'ads_seo'),
    ('seo',                  'ads_seo'),
    ('yandex-direct',        'ads_seo'),
    ('google-ads',           'ads_seo'),
    ('yandex-metrica',       'ads_seo'),
    ('google-analytics',     'ads_seo'),
    -- ai_creator: photoshop оставляем как пост-обработку нейро-картинок
    ('midjourney',           'ai_creator'),
    ('stable-diffusion',     'ai_creator'),
    ('runway',               'ai_creator'),
    ('sora',                 'ai_creator'),
    ('kling',                'ai_creator'),
    ('suno',                 'ai_creator'),
    ('prompt-engineering',   'ai_creator'),
    ('photoshop',            'ai_creator'),
    -- actor
    ('acting',               'actor'),
    ('voiceover',            'actor')
) AS v(slug, code) ON s.slug = v.slug
ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Откат частичный: убираем только связь категорий и навыков. CHECK на kind
-- НЕ возвращаем — иначе пришлось бы удалить из skills все 'skill'-строки, а
-- с ними и specialist_skills через ON DELETE CASCADE (потеря выбранных
-- навыков у живых специалистов). Если действительно нужно вернуть прод к
-- состоянию 00008 — делать отдельной ручной миграцией.
DROP TABLE IF EXISTS skill_categories;

-- +goose StatementEnd
