package leads

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound        = errors.New("lead not found")
	ErrRecipientMissing = errors.New("recipient not found")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

func (r *Repo) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
INSERT INTO leads (client_user_id, client_name, client_contact, brief, budget_min, budget_max, deadline, target_category_code)
VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
RETURNING id`,
		in.ClientUserID, in.ClientName, in.ClientContact, in.Brief,
		in.BudgetMin, in.BudgetMax, in.Deadline, in.TargetCategoryCode,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert lead: %w", err)
	}

	for _, sid := range in.SpecialistIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO lead_recipients (lead_id, specialist_user_id) VALUES ($1, $2)
             ON CONFLICT DO NOTHING`,
			id, sid); err != nil {
			return uuid.Nil, fmt.Errorf("insert recipient: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

func (r *Repo) ValidPublishedSpecialists(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM specialist_profiles WHERE user_id = ANY($1) AND is_published = TRUE`, ids)
	if err != nil {
		return nil, fmt.Errorf("validate specialists: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LoadSpecialistContacts — батчевый load имён + контактов для выбранных
// спецов. Используется только в ответе POST /leads (менеджер видит контакты
// уже после отправки брифа). Пустые поля = специалист их не заполнил в /me.
func (r *Repo) LoadSpecialistContacts(ctx context.Context, ids []uuid.UUID) ([]SpecialistContact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT user_id, display_name,
       COALESCE(contact_email, ''), COALESCE(contact_phone, '')
FROM specialist_profiles
WHERE user_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load specialist contacts: %w", err)
	}
	defer rows.Close()
	out := make([]SpecialistContact, 0, len(ids))
	for rows.Next() {
		var c SpecialistContact
		if err := rows.Scan(&c.UserID, &c.DisplayName, &c.ContactEmail, &c.ContactPhone); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repo) ListIncoming(ctx context.Context, specialistID uuid.UUID, status string, limit, offset int) ([]IncomingLead, error) {
	q := `
SELECT l.id, l.client_user_id, l.client_name, l.client_contact, l.brief,
       l.budget_min, l.budget_max, l.deadline,
       COALESCE(l.target_category_code, ''),
       l.status, l.created_at,
       (SELECT COUNT(*) FROM lead_recipients WHERE lead_id = l.id),
       lr.status, lr.responded_at
FROM leads l
JOIN lead_recipients lr ON lr.lead_id = l.id
WHERE lr.specialist_user_id = $1`
	args := []any{specialistID}
	if status != "" {
		q += ` AND lr.status = $2`
		args = append(args, status)
	}
	q += ` ORDER BY l.created_at DESC LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query incoming: %w", err)
	}
	defer rows.Close()

	out := make([]IncomingLead, 0, limit)
	for rows.Next() {
		var l IncomingLead
		if err := rows.Scan(
			&l.ID, &l.ClientUserID, &l.ClientName, &l.ClientContact, &l.Brief,
			&l.BudgetMin, &l.BudgetMax, &l.Deadline,
			&l.TargetCategoryCode, &l.Status, &l.CreatedAt,
			&l.RecipientCount,
			&l.RecipientStatus, &l.RecipientRespondedAt,
		); err != nil {
			return nil, fmt.Errorf("scan incoming: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *Repo) UpdateRecipientStatus(ctx context.Context, leadID, specialistID uuid.UUID, status string) error {
	tag, err := r.db.Exec(ctx, `
UPDATE lead_recipients
SET status = $3, responded_at = now()
WHERE lead_id = $1 AND specialist_user_id = $2`,
		leadID, specialistID, status)
	if err != nil {
		return fmt.Errorf("update recipient: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRecipientMissing
	}
	return nil
}

