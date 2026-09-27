-- +goose Up
-- +goose StatementBegin

-- Проектная страница: выкладки креаторов, ссылки на пять площадок, чеклист
-- перед сдачей и ежедневная статистика по каждому ролику.
-- Бизнес-требования — docs/PROJECT_PAGE.md.
--
-- Три вещи, которые определили схему:
--   1. Выкладка — отдельная сущность, а НЕ шаг воронки. У project_steps есть
--      weight и прогресс стадии; 60 выкладок в месяц сломали бы показатель
--      готовности проекта, а owner ('client'|'team'|'system') не выражает
--      «конкретный креатор».
--   2. Чеклист повторяет приём пайплайнов: шаблон с версией → снимок в проект
--      (см. 00010 и internal/projects/repo.go). Правка библиотеки не трогает
--      идущие проекты.
--   3. Статистика хранится снимком на дату, а не текущим значением. PK по
--      (link_id, stat_date) делает повторный сбор идемпотентным И физически
--      запрещает собрать один ролик дважды за сутки — это бизнес-правило,
--      а не оптимизация.

-- 1. Вид проекта. Их три и они ведут себя по-разному:
--   creators_turnkey    — креаторы под ключ: выкладки, 5 площадок, статистика
--   production_turnkey  — продакшн под ключ: воронка pipelines (уже есть)
--   general             — общий проект: клиент выбирает исполнителя и ставит
--                         один дедлайн, задача уходит в бот. Ни воронки,
--                         ни выкладок, ни статистики.
-- Значение по умолчанию — production_turnkey: все существующие проекты
-- созданы из воронки и ведутся менеджером, менять их поведение нельзя.
CREATE TYPE project_kind AS ENUM (
    'creators_turnkey', 'production_turnkey', 'general'
);

ALTER TABLE projects
    ADD COLUMN kind project_kind NOT NULL DEFAULT 'production_turnkey',
    -- due_date — единственный срок проекта вида general.
    ADD COLUMN due_date DATE,
    -- draft_required — этап черновика включается на уровне проекта, а не
    -- у каждой выкладки отдельно.
    ADD COLUMN draft_required BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN client_sees_stats BOOLEAN NOT NULL DEFAULT TRUE,
    -- monthly_plan — сколько роликов за месяц по договору (30 в первый
    -- месяц, 60 со второго). От него считается недостача: недосданное
    -- креатору не оплачивается.
    ADD COLUMN monthly_plan INT,
    -- collection_stops_at — когда прекращаем ежедневный сбор. Ставится
    -- в момент закрытия проекта: closed_at + 28 дней. После этой даты
    -- фоновая задача пишет итоговый снимок и удаляет ряд по дням.
    ADD COLUMN collection_stops_at TIMESTAMPTZ,
    ADD CONSTRAINT projects_monthly_plan_positive
        CHECK (monthly_plan IS NULL OR monthly_plan > 0);

-- Частичный индекс под выборку проектов-креаторов: планировщик статистики
-- и сводки менеджера ходят только по ним. Приём тот же, что у
-- projects_unassigned_idx в 00010.
CREATE INDEX projects_creators_idx ON projects(id)
    WHERE kind = 'creators_turnkey';

-- 2. Состав проекта. Креаторов в проекте несколько: их добавляет менеджер
-- из каталога и убирает из проекта. Убираем мягко (removed_at), а не
-- строкой DELETE: у выбывшего креатора остаются сданные выкладки и его
-- цифры в отчёте, и они не должны исчезнуть вместе со строкой состава.
CREATE TABLE project_creators (
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_by        UUID REFERENCES users(id),
    added_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at      TIMESTAMPTZ,
    PRIMARY KEY (project_id, creator_user_id)
);
-- «Мои проекты» у креатора: только те, откуда его не убрали.
CREATE INDEX project_creators_creator_idx
    ON project_creators(creator_user_id) WHERE removed_at IS NULL;

-- 3. Выкладка. Менеджер задаёт КОГДА, а не ЧТО: тему выбирает креатор по
-- брифу проекта.
--   planned         — дата стоит, ссылок нет
--   partial         — пришла часть площадок
--   done            — пришли все обязательные площадки
--   closed_manually — менеджер закрыл неполную выкладку руками, с причиной
--   cancelled       — выкладка отменена
CREATE TYPE publication_status AS ENUM (
    'planned', 'partial', 'done', 'closed_manually', 'cancelled'
);

CREATE TABLE project_publications (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users(id),
    due_date        DATE NOT NULL,
    -- draft_due_date заполняется, только если у проекта draft_required.
    draft_due_date  DATE,
    status          publication_status NOT NULL DEFAULT 'planned',
    -- closed_by / close_reason — только для closed_manually. Причина
    -- обязательна: закрытие неполной выкладки должно иметь автора и объяснение.
    closed_by       UUID REFERENCES users(id),
    close_reason    TEXT,
    -- created_batch_id — метка пачки. Даты ставятся десятками за одно
    -- действие, и отменять их надо тоже пачкой.
    created_batch_id UUID,
    created_by      UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT publications_closed_has_reason CHECK (
        status <> 'closed_manually'
        OR (closed_by IS NOT NULL AND close_reason IS NOT NULL AND close_reason <> '')
    ),
    CONSTRAINT publications_draft_before_due CHECK (
        draft_due_date IS NULL OR draft_due_date <= due_date
    )
);
CREATE INDEX publications_project_idx ON project_publications(project_id, due_date);
CREATE INDEX publications_creator_idx ON project_publications(creator_user_id, due_date);
-- Узкий индекс под утренний обход напоминаний: берём только незакрытые.
CREATE INDEX publications_open_idx ON project_publications(due_date)
    WHERE status IN ('planned', 'partial');
CREATE INDEX publications_batch_idx ON project_publications(created_batch_id)
    WHERE created_batch_id IS NOT NULL;
-- Одна выкладка на пару «креатор + день». Защита от двойного клика на
-- массовом создании: без неё повторная отправка формы давала вторую
-- пачку на те же даты, CancelBatch снимал только одну, а по второй
-- бесконечно шли напоминания. Отменённые из-под ограничения выведены —
-- после отмены на ту же дату можно поставить заново.
CREATE UNIQUE INDEX publications_creator_day_uniq
    ON project_publications(project_id, creator_user_id, due_date)
    WHERE status <> 'cancelled';

-- 4. Ссылки на опубликованный ролик. Один ролик идёт на пять площадок,
-- значит на выкладку приходится до пяти ссылок.
--
-- platform — TEXT + CHECK, а не ENUM: набор площадок ещё может измениться
-- (по Likee нет публичного API, и он может выпасть из обязательных).
-- Менять CHECK дешевле, чем ALTER TYPE ... ADD VALUE, который необратим.
CREATE TABLE publication_links (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publication_id UUID NOT NULL REFERENCES project_publications(id) ON DELETE CASCADE,
    platform       TEXT NOT NULL
        CHECK (platform IN ('tiktok', 'instagram', 'youtube', 'vk', 'likee')),
    -- url — то, что вставил креатор, как есть (с рекламными хвостами,
    -- мобильным доменом). Храним для разбирательств.
    url            TEXT NOT NULL,
    -- url_canonical — очищенная ссылка, по ней ходим в instacurl.
    url_canonical  TEXT NOT NULL,
    -- external_media_id — id ролика на площадке, если удалось выделить.
    external_media_id TEXT,
    submitted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Расписание сбора. Интервал удваивается по мере старения ролика:
    -- 0-5 дней — каждый день, 6-14 — раз в 2, 15-28 — раз в 4, дальше 8.
    -- next_collect_at NULL = снят с обхода (проект закрыт больше 28 дней).
    collect_interval_days INT NOT NULL DEFAULT 1,
    next_collect_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_collected_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Одна ссылка на площадку: пять площадок — пять строк, не больше.
    UNIQUE (publication_id, platform),
    CONSTRAINT links_interval_positive CHECK (collect_interval_days > 0)
);
CREATE INDEX links_publication_idx ON publication_links(publication_id);
-- Очередь сбора: планировщик берёт «пора собирать» по времени.
CREATE INDEX links_due_collect_idx ON publication_links(next_collect_at);

-- 5. Просьба креатора о переносе даты. Дедлайн ставит менеджер, креатор
-- его не меняет — только просит. Пока просьба pending, выкладка НЕ
-- считается просроченной (иначе пинги идут по человеку, который уже
-- предупредил).
CREATE TABLE publication_date_requests (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publication_id UUID NOT NULL REFERENCES project_publications(id) ON DELETE CASCADE,
    requested_date DATE NOT NULL,
    reason         TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected')),
    decided_by     UUID REFERENCES users(id),
    decided_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Одна открытая просьба на выкладку: вторую заводить незачем.
CREATE UNIQUE INDEX date_requests_one_pending_idx
    ON publication_date_requests(publication_id) WHERE status = 'pending';

-- 6. Библиотека чеклистов. Живёт ВНЕ проекта и версионируется — как
-- pipelines. В проект попадает копией (см. project_checklist_items).
CREATE TABLE checklist_templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    version     INT  NOT NULL DEFAULT 1,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX checklist_templates_active_name_idx
    ON checklist_templates(LOWER(name)) WHERE is_active = TRUE;

CREATE TABLE checklist_template_items (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES checklist_templates(id) ON DELETE CASCADE,
    text        TEXT NOT NULL,
    -- platform NULL = общий пункт. Иначе пункт показывается только для
    -- своей площадки: требования к описанию в TikTok и YouTube разные.
    platform    TEXT
        CHECK (platform IS NULL OR platform IN ('tiktok', 'instagram', 'youtube', 'vk', 'likee')),
    is_required BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order  INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX checklist_template_items_tpl_idx ON checklist_template_items(template_id, sort_order);

-- 7. Снимок чеклиста в проекте. Правка библиотеки не меняет идущие проекты.
CREATE TABLE project_checklist_items (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- source_item_id — откуда скопировано. Только для истории; библиотека
    -- может быть переписана, поэтому ON DELETE SET NULL.
    source_item_id UUID REFERENCES checklist_template_items(id) ON DELETE SET NULL,
    text        TEXT NOT NULL,
    platform    TEXT
        CHECK (platform IS NULL OR platform IN ('tiktok', 'instagram', 'youtube', 'vk', 'likee')),
    is_required BOOLEAN NOT NULL,
    sort_order  INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_checklist_items_project_idx ON project_checklist_items(project_id, sort_order);

-- 8. Отметки креатора перед сдачей. Обязательные непройденные пункты
-- блокируют отправку — проверка на бэке, не только на фронте.
CREATE TABLE publication_checklist_marks (
    publication_id UUID NOT NULL REFERENCES project_publications(id) ON DELETE CASCADE,
    item_id        UUID NOT NULL REFERENCES project_checklist_items(id) ON DELETE CASCADE,
    checked_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    checked_by     UUID REFERENCES users(id),
    PRIMARY KEY (publication_id, item_id)
);

-- 9. Ежедневный снимок метрик ролика.
-- PK (link_id, stat_date) — тройная защита: повторный сбор за тот же день
-- идемпотентен (upsert), пропущенный день не искажает историю, и один
-- ролик физически нельзя собрать дважды в сутки.
CREATE TABLE video_stat_daily (
    link_id      UUID NOT NULL REFERENCES publication_links(id) ON DELETE CASCADE,
    stat_date    DATE NOT NULL,
    views        BIGINT,
    likes        BIGINT,
    comments     BIGINT,
    -- collected_at — когда реально сходили. Показывается пользователю как
    -- «дата последнего сбора»: без неё возникает вопрос, почему цифра не
    -- изменилась за час.
    collected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (link_id, stat_date)
);
CREATE INDEX video_stat_daily_date_idx ON video_stat_daily(stat_date);

-- 10. Итоговый снимок проекта. Через 28 дней после закрытия ежедневный ряд
-- схлопывается сюда и удаляется: клиенту остаются просмотры навсегда,
-- а история по дням больше не нужна.
-- ВНИМАНИЕ: удаление ряда необратимо. Снимок обязан быть записан ДО
-- удаления, в одной транзакции (см. тест collapse_writes_summary_first).
CREATE TABLE project_stat_summary (
    project_id   UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    views        BIGINT NOT NULL DEFAULT 0,
    likes        BIGINT NOT NULL DEFAULT 0,
    comments     BIGINT NOT NULL DEFAULT 0,
    videos_count INT    NOT NULL DEFAULT 0,
    -- as_of — дата, на которую зафиксированы цифры.
    as_of        DATE NOT NULL,
    collapsed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 11. Материалы проекта: бренд-гайд, обучение, ссылки на аккаунты.
-- Видит команда проекта; клиенту не показываются.
CREATE TABLE project_materials (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('doc', 'video', 'link')),
    title      TEXT NOT NULL,
    url        TEXT NOT NULL,
    sort_order INT  NOT NULL DEFAULT 0,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_materials_project_idx ON project_materials(project_id, sort_order);

-- 12. Журнал отправленных уведомлений.
-- Требование: одно и то же напоминание не приходит дважды за день, даже
-- если воркер перезапустился. Держим это уникальным индексом в БД, а не
-- памятью процесса — перезапуск память обнуляет.
CREATE TABLE notification_log (
    id         BIGSERIAL PRIMARY KEY,
    -- user_id NULL = получатель не человек, а общий чат менеджеров: туда
    -- уходит сводка «что горит и по кому». Персональные напоминания
    -- креатору всегда с user_id.
    user_id    UUID REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    -- subject_id — на что напоминаем (publication_id, а для сводки —
    -- project_id). Без FK: журнал переживает удаление предмета.
    subject_id UUID NOT NULL,
    sent_date  DATE NOT NULL DEFAULT CURRENT_DATE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Защита от повторной отправки. Через COALESCE, а не обычным UNIQUE:
-- в уникальном индексе Postgres считает NULL'ы различными, и сводки в
-- общий чат дедуплицировались бы только по видимости — при каждом
-- перезапуске воркера уходила бы новая.
-- Уведомление «ролик перешагнул порог просмотров» шлётся ОДИН раз за всю
-- жизнь ролика, а не раз в сутки. Дедуп по дате для него не годится:
-- на границе суток два прохода получают разные ключи и оба проходят.
-- Здесь дата из ключа исключена намеренно.
CREATE UNIQUE INDEX notification_log_threshold_uniq
    ON notification_log(user_id, kind, subject_id)
    WHERE kind = 'client_views_threshold';

CREATE UNIQUE INDEX notification_log_dedup_idx ON notification_log (
    COALESCE(user_id, '00000000-0000-0000-0000-000000000000'::uuid),
    kind, subject_id, sent_date
);

-- 13. Настройки уведомлений клиента по проекту.
--
-- Требование Кл1: «Настройки уведомлений — его». Клиент сам решает, о чём
-- ему писать: вышел новый ролик, недельная сводка, сдвинулась дата,
-- ролик перешагнул порог просмотров.
--
-- Строка на пару (проект, клиент), а не на клиента: у разных проектов
-- разный темп, и человек, ведущий два, вполне может хотеть ежедневных
-- уведомлений по одному и недельной сводки по другому.
CREATE TABLE client_notification_prefs (
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    on_new_video    BOOLEAN NOT NULL DEFAULT TRUE,
    on_weekly_digest BOOLEAN NOT NULL DEFAULT TRUE,
    on_date_shift   BOOLEAN NOT NULL DEFAULT TRUE,
    -- views_threshold — «ролик перешагнул порог просмотров». NULL = не
    -- уведомлять. Порог задаёт сам клиент: для одного проекта событие —
    -- это 100 тысяч, для другого миллион.
    views_threshold BIGINT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id),
    CONSTRAINT client_prefs_threshold_positive
        CHECK (views_threshold IS NULL OR views_threshold > 0)
);

-- 14. Привязка Telegram к пользователю. Доставка идёт через n8n, но сама
-- привязка живёт у нас: её нельзя терять и нельзя отдавать наружу.
CREATE TABLE bot_links (
    user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    chat_id    TEXT NOT NULL,
    linked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chat_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS bot_links;
DROP TABLE IF EXISTS client_notification_prefs;
DROP TABLE IF EXISTS notification_log;
DROP TABLE IF EXISTS project_materials;
DROP TABLE IF EXISTS project_stat_summary;
DROP TABLE IF EXISTS video_stat_daily;
DROP TABLE IF EXISTS publication_checklist_marks;
DROP TABLE IF EXISTS project_checklist_items;
DROP TABLE IF EXISTS checklist_template_items;
DROP TABLE IF EXISTS checklist_templates;
DROP TABLE IF EXISTS publication_date_requests;
DROP TABLE IF EXISTS publication_links;
DROP TABLE IF EXISTS project_publications;
DROP TABLE IF EXISTS project_creators;

DROP TYPE IF EXISTS publication_status;

DROP INDEX IF EXISTS projects_creators_idx;
ALTER TABLE projects
    DROP CONSTRAINT IF EXISTS projects_monthly_plan_positive,
    DROP COLUMN IF EXISTS collection_stops_at,
    DROP COLUMN IF EXISTS monthly_plan,
    DROP COLUMN IF EXISTS client_sees_stats,
    DROP COLUMN IF EXISTS draft_required,
    DROP COLUMN IF EXISTS due_date,
    DROP COLUMN IF EXISTS kind;

DROP TYPE IF EXISTS project_kind;

-- +goose StatementEnd
