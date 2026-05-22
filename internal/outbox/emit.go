package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	AggregateSpecialist = "specialist"
	AggregateEmail      = "email"

	EventSpecialistUpserted  = "specialist.upserted"
	EventSpecialistPublished = "specialist.published"
	EventSpecialistRetracted = "specialist.retracted"
	EventSpecialistDeleted   = "specialist.deleted"

	// EventEmailVerifySend — payload: {to, to_name, token, base_url}.
	// Воркер на это событие шлёт письмо подтверждения через mailer.Sender.
	EventEmailVerifySend = "email.verify_send"
)

// EmailVerifyPayload — структура payload для EventEmailVerifySend.
// Объявлено в outbox, чтобы и emitter (auth.Service) и handler (cmd/worker)
// зависели от одной формы.
type EmailVerifyPayload struct {
	To      string `json:"to"`
	ToName  string `json:"to_name,omitempty"`
	Token   string `json:"token"`    // raw токен (нешифрованный) — для вставки в URL
	BaseURL string `json:"base_url"` // публичный URL фронта (APP_BASE_URL)
}

func Emit(ctx context.Context, tx pgx.Tx, aggregate, aggregateID, eventType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	const q = `
INSERT INTO outbox (aggregate, aggregate_id, event_type, payload)
VALUES ($1, $2, $3, $4)`
	if _, err := tx.Exec(ctx, q, aggregate, aggregateID, eventType, data); err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}
	return nil
}
