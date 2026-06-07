-- +goose Up
-- support_messages — обращения из футера UI «Написать в поддержку».
-- Гости могут писать (user_id NULL); залогиненым подставляется auto.
-- topic — выпадашка на фронте (payment/bug/feature/other); храним свободной
-- строкой чтобы добавлять опции без миграции.
CREATE TABLE support_messages (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid REFERENCES users(id) ON DELETE SET NULL,
    from_email text NOT NULL,
    from_name  text,
    topic      text NOT NULL,
    message    text NOT NULL CHECK (length(message) BETWEEN 10 AND 2000),
    source_url text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX support_messages_created_idx ON support_messages (created_at DESC);

-- +goose Down
DROP TABLE support_messages;
