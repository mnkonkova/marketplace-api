-- +goose Up
-- +goose StatementBegin

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         CITEXT UNIQUE,
    phone         TEXT UNIQUE,
    password_hash TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('client', 'specialist', 'both')),
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (email IS NOT NULL OR phone IS NOT NULL)
);

CREATE TABLE specialist_profiles (
    user_id        UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    display_name   TEXT NOT NULL,
    bio            TEXT NOT NULL DEFAULT '',
    avatar_url     TEXT,
    city           TEXT,
    rate_min       INTEGER,
    rate_max       INTEGER,
    currency       TEXT NOT NULL DEFAULT 'RUB',
    is_published   BOOLEAN NOT NULL DEFAULT FALSE,
    rating_avg     NUMERIC(3,2) NOT NULL DEFAULT 0,
    reviews_count  INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (rate_min IS NULL OR rate_max IS NULL OR rate_min <= rate_max)
);

CREATE TABLE specialty_categories (
    code        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    type        TEXT NOT NULL DEFAULT '',
    icon        TEXT,
    sort_order  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE specialist_categories (
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category_code TEXT NOT NULL REFERENCES specialty_categories(code) ON DELETE RESTRICT,
    is_primary    BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (user_id, category_code)
);

CREATE UNIQUE INDEX specialist_categories_one_primary
    ON specialist_categories(user_id) WHERE is_primary;

CREATE TABLE skills (
    id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug  TEXT UNIQUE NOT NULL,
    title TEXT NOT NULL,
    kind  TEXT NOT NULL CHECK (kind IN ('tool', 'platform', 'genre'))
);

CREATE TABLE specialist_skills (
    user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    skill_id UUID NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, skill_id)
);

CREATE TABLE portfolio_items (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title          TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    video_url      TEXT,
    thumbnail_url  TEXT,
    external_url   TEXT,
    category_codes TEXT[] NOT NULL DEFAULT '{}',
    sort_order     INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX portfolio_items_user_idx ON portfolio_items(user_id);
-- Горячий путь /feed.LoadVideosByUsers: фильтр user_id ∈ (...) с kind=video и
-- видимым video_url, потом row_number() ORDER BY sort_order, created_at DESC
-- на разбиение по спецу. Покрывающий индекс убирает heap-scan + сразу даёт
-- готовый порядок внутри партиции.
CREATE INDEX portfolio_items_videos_idx
    ON portfolio_items(user_id, sort_order, created_at DESC)
    WHERE kind = 'video' AND video_url IS NOT NULL AND video_url <> '';

CREATE TABLE leads (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_user_id       UUID REFERENCES users(id) ON DELETE SET NULL,
    client_name          TEXT NOT NULL,
    client_contact       TEXT NOT NULL,
    brief                TEXT NOT NULL,
    budget_min           INTEGER,
    budget_max           INTEGER,
    deadline             DATE,
    target_category_code TEXT REFERENCES specialty_categories(code) ON DELETE SET NULL,
    status               TEXT NOT NULL DEFAULT 'open'
                          CHECK (status IN ('open', 'in_progress', 'closed', 'cancelled')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX leads_status_idx ON leads(status);
CREATE INDEX leads_target_category_idx ON leads(target_category_code);

CREATE TABLE lead_recipients (
    lead_id             UUID NOT NULL REFERENCES leads(id) ON DELETE CASCADE,
    specialist_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status              TEXT NOT NULL DEFAULT 'sent'
                         CHECK (status IN ('sent', 'viewed', 'accepted', 'declined')),
    responded_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (lead_id, specialist_user_id)
);
CREATE INDEX lead_recipients_specialist_idx ON lead_recipients(specialist_user_id, status);

CREATE TABLE reviews (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lead_id         UUID REFERENCES leads(id) ON DELETE SET NULL,
    author_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    rating          SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    text            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (lead_id, author_user_id, target_user_id)
);
CREATE INDEX reviews_target_idx ON reviews(target_user_id);

CREATE TABLE outbox (
    id            BIGSERIAL PRIMARY KEY,
    aggregate     TEXT NOT NULL,
    aggregate_id  TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ
);
CREATE INDEX outbox_unprocessed_idx ON outbox(created_at) WHERE processed_at IS NULL;

INSERT INTO specialty_categories (code, title, description, type, sort_order) VALUES
    ('editor',         'Монтажёр',                    'Видеомонтаж, нарезка, цветокор',                        'Производство', 10),
    ('video_director', 'Видеоредактор / режиссёр монтажа', 'Концепция, сторителлинг, режиссура монтажа',  'Производство', 20),
    ('motion',         'Моушн-дизайнер',              'After Effects, анимация, графика, титры',                'Производство', 30),
    ('scriptwriter',   'Сценарист',                   'Сценарии для роликов, шортсов, рекламы',                 'Производство', 40),
    ('ugc',            'UGC-контент',                 'Создание UGC-роликов под бренды',                        'Производство', 50),
    ('videographer',   'Видеооператор',               'Съёмка видео: рекламные ролики, репортаж, интервью',    'Производство', 60),
    ('photographer',   'Фотограф',                    'Предметная, портретная, репортажная съёмка',             'Производство', 70),
    ('actor',          'Актёр',                       'Съёмки в рекламе, UGC, роликах, озвучка',                'Производство', 80),
    ('designer',       'Дизайнер',                    'Графический дизайн, креативы, обложки, превью',          'Производство', 90),
    ('ai_creator',     'ИИ-креатор',                  'Генерация видео/фото/звука через нейросети, промптинг',  'Производство', 100),
    ('smm',            'СММ',                         'Ведение соцсетей, контент-планы',                        'Продвижение',  10000),
    ('blogger',        'Блогер',                      'Интеграции, нативная реклама',                           'Продвижение',  10010),
    ('ads_seo',        'Таргет + SEO',                'Настройка таргетированной рекламы и SEO-продвижение',    'Продвижение',  10020),
    ('seeding',        'Посевы',                      'Посевы в каналах и пабликах',                            'Продвижение',  10030);

INSERT INTO skills (slug, title, kind) VALUES
    ('premiere',     'Adobe Premiere Pro',  'tool'),
    ('after-effects','After Effects',       'tool'),
    ('davinci',      'DaVinci Resolve',     'tool'),
    ('final-cut',    'Final Cut Pro',       'tool'),
    ('capcut',       'CapCut',              'tool'),
    ('photoshop',    'Photoshop',           'tool'),
    ('figma',        'Figma',               'tool'),
    ('reels',        'Instagram Reels',     'platform'),
    ('tiktok',       'TikTok',              'platform'),
    ('youtube',      'YouTube',             'platform'),
    ('shorts',       'YouTube Shorts',      'platform'),
    ('vk-clips',     'VK Клипы',            'platform'),
    ('telegram',     'Telegram',            'platform');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS reviews;
DROP TABLE IF EXISTS lead_recipients;
DROP TABLE IF EXISTS leads;
DROP TABLE IF EXISTS portfolio_items;
DROP TABLE IF EXISTS specialist_skills;
DROP TABLE IF EXISTS skills;
DROP TABLE IF EXISTS specialist_categories;
DROP TABLE IF EXISTS specialty_categories;
DROP TABLE IF EXISTS specialist_profiles;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
