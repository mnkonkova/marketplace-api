-- +goose Up
-- +goose StatementBegin

-- Напоминание НАКАНУНЕ выкладки.
--
-- До сих пор бот писал креатору утром в день срока — то есть в день,
-- когда снимать и монтировать уже поздно. «Завтра срок» — это ещё
-- рабочее напоминание, а «сегодня срок» — уже констатация. В плане
-- выкладок у менеджера напротив каждого креатора стоит колокольчик,
-- и включает он именно это.
--
-- По умолчанию ВЫКЛЮЧЕНО, в отличие от остальных трёх видов. Остальные
-- включены, потому что были с самого начала и на них рассчитывают;
-- этот появляется сейчас, и включать его молча всем — значит завтра
-- утром прислать новое письмо каждому креатору каждого проекта, никого
-- не спросив.
ALTER TABLE project_reminder_prefs
    ADD COLUMN IF NOT EXISTS day_before BOOLEAN NOT NULL DEFAULT FALSE;

-- Колокольчик стоит НАПРОТИВ КРЕАТОРА, а не напротив проекта: в плане
-- у восьми человек восемь разных строк, и включают напоминание тому,
-- кто забывает, а не всем сразу.
--
-- Строка здесь — исключение из настройки проекта: нет строки, значит
-- действует project_reminder_prefs.day_before.
CREATE TABLE IF NOT EXISTS project_creator_reminder_prefs (
    project_id      UUID    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    creator_user_id UUID    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day_before      BOOLEAN NOT NULL,
    updated_by      UUID REFERENCES users(id),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, creator_user_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS project_creator_reminder_prefs;
ALTER TABLE project_reminder_prefs DROP COLUMN IF EXISTS day_before;

-- +goose StatementEnd
