// Package support — приём обращений в службу поддержки из футера UI.
// Создаёт запись в support_messages + outbox-событие support.message_received,
// которое worker отдаёт в n8n (дальше в Telegram-чат поддержки).
package support

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
)

var (
	ErrInvalidEmail   = errors.New("invalid email")
	ErrInvalidMessage = errors.New("message must be 10-2000 chars")
)

var allowedTopics = map[string]bool{
	"payment": true, "bug": true, "feature": true, "other": true,
}

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

type CreateInput struct {
	UserID    *uuid.UUID // nil для гостей
	FromEmail string
	FromName  string
	Topic     string
	Message   string
	SourceURL string
}

// Create — пишет support_message + outbox event в одной транзакции.
// Возвращает id записи.
func (s *Service) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	in.FromEmail = strings.TrimSpace(in.FromEmail)
	in.Message = strings.TrimSpace(in.Message)
	in.Topic = strings.ToLower(strings.TrimSpace(in.Topic))
	if _, err := mail.ParseAddress(in.FromEmail); err != nil {
		return uuid.Nil, ErrInvalidEmail
	}
	if n := len(in.Message); n < 10 || n > 2000 {
		return uuid.Nil, ErrInvalidMessage
	}
	if !allowedTopics[in.Topic] {
		in.Topic = "other"
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
INSERT INTO support_messages (user_id, from_email, from_name, topic, message, source_url)
VALUES (NULLIF($1,'00000000-0000-0000-0000-000000000000'::uuid), $2, NULLIF($3,''), $4, $5, NULLIF($6,''))
RETURNING id`,
		userIDOrNil(in.UserID), in.FromEmail, in.FromName, in.Topic, in.Message, in.SourceURL,
	).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("insert: %w", err)
	}

	payload := map[string]any{
		"message_id": id.String(),
		"from_email": in.FromEmail,
		"from_name":  in.FromName,
		"topic":      in.Topic,
		"message":    in.Message,
		"source_url": in.SourceURL,
	}
	if in.UserID != nil {
		payload["user_id"] = in.UserID.String()
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateSupport, id.String(),
		outbox.EventSupportMessageReceived, payload); err != nil {
		return uuid.Nil, fmt.Errorf("emit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

func userIDOrNil(u *uuid.UUID) uuid.UUID {
	if u == nil {
		return uuid.Nil
	}
	return *u
}
