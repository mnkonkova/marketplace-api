package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	AggregateSpecialist = "specialist"

	EventSpecialistUpserted   = "specialist.upserted"
	EventSpecialistPublished  = "specialist.published"
	EventSpecialistRetracted  = "specialist.retracted"
	EventSpecialistDeleted    = "specialist.deleted"
)

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
