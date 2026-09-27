-- +goose Up
-- +goose StatementBegin

-- Чем снимок чеклиста связан с шаблоном.
--
-- Пункты копируются в project_checklist_items со ссылкой source_item_id,
-- и по ней теоретически можно дойти до шаблона. Практически — нельзя:
-- ссылка стоит ON DELETE SET NULL, а версия шаблона живёт колонкой в той
-- же строке и переписывается при правке библиотеки. То есть узнать, какая
-- версия была подключена, уже невозможно — а именно это и надо показать:
-- «подключён v3, в библиотеке вышла v4».
--
-- Поэтому имя и версия сохраняются СНИМКОМ, рядом с самими пунктами.
CREATE TABLE project_checklist_snapshot (
    project_id       UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    -- template_id — на что ссылались. SET NULL: библиотеку могут почистить,
    -- а снимок в проекте от этого не портится.
    template_id      UUID REFERENCES checklist_templates(id) ON DELETE SET NULL,
    -- Имя и версия — копии на момент подключения, а не текущие значения.
    template_name    TEXT NOT NULL,
    template_version INT  NOT NULL,
    connected_by     UUID REFERENCES users(id),
    connected_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS project_checklist_snapshot;

-- +goose StatementEnd
