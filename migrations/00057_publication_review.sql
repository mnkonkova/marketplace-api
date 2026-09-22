-- +goose Up
-- Проверка ролика менеджером.
--
-- До сих пор чек-лист существовал только со стороны креатора: он сам
-- отмечал пункты при сдаче, бэк не давал сдать с непройденными
-- обязательными — и на этом всё заканчивалось. Кто-то посмотрел ли
-- ролик на самом деле, нигде не хранилось, и «вернуть переснять» жило
-- в переписке. Здесь это становится данными: по каждому пункту стоит
-- «да» или «нет» ИМЕНЕМ менеджера, а решение по ролику — принят или
-- возвращён с замечанием — видно и менеджеру, и креатору одинаково.
--
-- Отметка креатора (publication_checklist_marks) НЕ заменяется: это
-- разные утверждения. «Я сделал» и «я проверил» должны расходиться,
-- когда расходятся, — иначе проверка не нужна.

CREATE TABLE publication_reviews (
    publication_id UUID PRIMARY KEY REFERENCES project_publications(id) ON DELETE CASCADE,
    -- in_review — проверка идёт, решения ещё нет;
    -- returned   — возвращено креатору с замечанием;
    -- accepted   — принято.
    status      TEXT NOT NULL
        CHECK (status IN ('in_review', 'returned', 'accepted')),
    -- Замечание последнего решения. Обязательно при возврате: «вернули
    -- молча» креатор всё равно придёт спрашивать словами.
    comment     TEXT NOT NULL DEFAULT '',
    -- round — какая это по счёту сдача. Больше единицы означает, что
    -- ролик уже возвращали; отдельного счётчика возвратов не нужно.
    round       INT NOT NULL DEFAULT 1,
    decided_by  UUID REFERENCES users(id),
    decided_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Вердикты по пунктам чек-листа. Хранится и «да», и «нет»: непройденный
-- пункт — это содержание замечания, а не отсутствие отметки. Пункт, по
-- которому менеджер ещё не решил, строки просто не имеет.
CREATE TABLE publication_review_marks (
    publication_id UUID NOT NULL REFERENCES project_publications(id) ON DELETE CASCADE,
    item_id        UUID NOT NULL REFERENCES project_checklist_items(id) ON DELETE CASCADE,
    passed         BOOLEAN NOT NULL,
    marked_by      UUID REFERENCES users(id),
    marked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (publication_id, item_id)
);

CREATE INDEX publication_review_marks_item_idx ON publication_review_marks(item_id);

-- +goose Down
DROP TABLE publication_review_marks;
DROP TABLE publication_reviews;
