-- +goose Up
-- +goose StatementBegin
ALTER TABLE reviews ADD COLUMN author_name TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE reviews DROP COLUMN author_name;
-- +goose StatementEnd
