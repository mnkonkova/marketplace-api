-- +goose Up
-- +goose StatementBegin

-- План роликов в тарифе.
--
-- В карточке «Оклад 60 000 ₽» стоит подпись «30 видео первый месяц,
-- 60 видео/мес со второго» — то есть оклад назван не сам по себе, а за
-- объём. Без этих чисел карточку не собрать, а брать их из projects.
-- monthly_plan нельзя: у заказа проекта ещё нет, а смету показывать надо.
ALTER TABLE terms_versions
    ADD COLUMN videos_first_month INT NOT NULL DEFAULT 0,
    ADD COLUMN videos_next_months INT NOT NULL DEFAULT 0,
    ADD CONSTRAINT terms_video_plan_non_negative
        CHECK (videos_first_month >= 0 AND videos_next_months >= 0);

ALTER TABLE project_billing
    ADD COLUMN videos_first_month INT NOT NULL DEFAULT 0,
    ADD COLUMN videos_next_months INT NOT NULL DEFAULT 0,
    ADD CONSTRAINT project_billing_video_plan_non_negative
        CHECK (videos_first_month >= 0 AND videos_next_months >= 0);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE project_billing
    DROP CONSTRAINT IF EXISTS project_billing_video_plan_non_negative,
    DROP COLUMN IF EXISTS videos_next_months,
    DROP COLUMN IF EXISTS videos_first_month;

ALTER TABLE terms_versions
    DROP CONSTRAINT IF EXISTS terms_video_plan_non_negative,
    DROP COLUMN IF EXISTS videos_next_months,
    DROP COLUMN IF EXISTS videos_first_month;

-- +goose StatementEnd
