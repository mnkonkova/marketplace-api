-- +goose Up
-- +goose StatementBegin

-- Заявка заказчика на следующий месяц.
--
-- В кабинете заказчика есть прикидка «во сколько обойдётся следующий
-- месяц»: ползунки роликов и креаторов и одно число сверху. До сих пор
-- она была справочной — посмотрел и закрыл, — а дальше человек шёл
-- писать менеджеру словами, и половина не доходила вовсе.
--
-- Кнопка «Заказать» превращает прикидку в заявку: менеджер видит её
-- плашкой в проекте и получает сообщение в общий чат. Заявка — это ещё
-- не заказ: цену и состав финализирует менеджер, а здесь записано, о
-- чём именно попросили и какое число человек видел на экране.
--
-- Отдельная таблица, а не колонки в projects: заявка — событие со своей
-- датой и своим «разобрали», и вторая заявка через месяц не должна
-- затирать первую. История ответов на вопрос «о чём договаривались»
-- живёт здесь.
CREATE TABLE project_month_requests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    requested_by UUID NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    -- Какой месяц просят: первое число. Заявку оставляют заранее, и
    -- «следующий» через неделю значит уже другой месяц.
    month        DATE   NOT NULL,
    -- Что стояло на ползунках и какое число человек видел сверху.
    -- Ceiling храним снимком: прайс поменяется, а разговор пойдёт о той
    -- сумме, которую ему показали.
    creators     INT    NOT NULL CHECK (creators > 0),
    videos       INT    NOT NULL CHECK (videos >= 0),
    ceiling      BIGINT NOT NULL CHECK (ceiling >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Разобрали: менеджер связался и завёл заказ (или отказал). Пустое —
    -- заявка висит, и плашка в проекте горит.
    handled_at   TIMESTAMPTZ,
    handled_by   UUID REFERENCES users(id)
);

-- Открытая заявка у проекта одна. Вторая кнопка «Заказать» до разбора
-- первой — это уточнение той же просьбы, а не новая: иначе у менеджера
-- копится стопка одинаковых плашек, и разбирать он будет верхнюю.
CREATE UNIQUE INDEX project_month_requests_open_uniq
    ON project_month_requests(project_id) WHERE handled_at IS NULL;

CREATE INDEX project_month_requests_project_idx
    ON project_month_requests(project_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_month_requests;
-- +goose StatementEnd
