package reviews

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
)

var (
	ErrNotFound  = errors.New("review not found")
	ErrForbidden = errors.New("not the author")
	ErrLeadCheck = errors.New("lead does not authorize this review")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// TargetIsPublishedSpecialist — target должен быть профилем спеца.
// is_published проверять не нужно (отзыв ставится по факту работы, профиль
// мог временно сняться с публикации), но запись в specialist_profiles обязана.
func (r *Repo) TargetIsSpecialist(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM specialist_profiles WHERE user_id = $1)`, id,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check target: %w", err)
	}
	return ok, nil
}

// LeadAuthorizesReview — клиент мог оставить отзыв по этому lead'у только
// если он же создавал заявку и target есть в lead_recipients со статусом
// accepted. UNIQUE(lead_id, author_user_id, target_user_id) в таблице
// reviews не даст двух отзывов по одной паре.
func (r *Repo) LeadAuthorizesReview(ctx context.Context, leadID, authorID, targetID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1
  FROM leads l
  JOIN lead_recipients lr ON lr.lead_id = l.id
  WHERE l.id = $1
    AND l.client_user_id = $2
    AND lr.specialist_user_id = $3
    AND lr.status = 'accepted'
)`, leadID, authorID, targetID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check lead authorization: %w", err)
	}
	return ok, nil
}

func (r *Repo) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
INSERT INTO reviews (lead_id, author_user_id, author_name, target_user_id, rating, text)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`,
		in.LeadID, in.AuthorUserID, in.AuthorName, in.TargetUserID, in.Rating, in.Text,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert review: %w", err)
	}

	// Триггер reviews_recalc_trg уже обновил rating_avg/reviews_count в той
	// же транзакции, осталось дёрнуть outbox чтобы worker перечитал
	// документ в OpenSearch (там лежат те же поля для сортировки/буста).
	if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, in.TargetUserID.String(),
		outbox.EventSpecialistUpserted, map[string]string{"user_id": in.TargetUserID.String()}); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

func (r *Repo) Update(ctx context.Context, id, authorID uuid.UUID, in UpdateInput) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var target uuid.UUID
	err = tx.QueryRow(ctx, `
UPDATE reviews SET
  rating = COALESCE($3, rating),
  text   = COALESCE($4, text)
WHERE id = $1 AND author_user_id = $2
RETURNING target_user_id`,
		id, authorID, in.Rating, in.Text,
	).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		// Либо отзыва нет, либо автор не он. Различаем доп. запросом —
		// 404 vs 403 информативнее, чем общий 404.
		var exists bool
		if err := r.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM reviews WHERE id = $1)`, id).Scan(&exists); err != nil {
			return uuid.Nil, fmt.Errorf("probe review: %w", err)
		}
		if !exists {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, ErrForbidden
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("update review: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, target.String(),
		outbox.EventSpecialistUpserted, map[string]string{"user_id": target.String()}); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return target, nil
}

func (r *Repo) Delete(ctx context.Context, id, authorID uuid.UUID) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var target uuid.UUID
	err = tx.QueryRow(ctx, `
DELETE FROM reviews WHERE id = $1 AND author_user_id = $2
RETURNING target_user_id`, id, authorID).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := r.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM reviews WHERE id = $1)`, id).Scan(&exists); err != nil {
			return uuid.Nil, fmt.Errorf("probe review: %w", err)
		}
		if !exists {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, ErrForbidden
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("delete review: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, target.String(),
		outbox.EventSpecialistUpserted, map[string]string{"user_id": target.String()}); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return target, nil
}

func (r *Repo) GetByID(ctx context.Context, id uuid.UUID) (Review, error) {
	var rv Review
	err := r.db.QueryRow(ctx, `
SELECT id, lead_id, author_user_id, author_name, target_user_id, rating, text, created_at
FROM reviews WHERE id = $1`, id).Scan(
		&rv.ID, &rv.LeadID, &rv.AuthorUserID, &rv.AuthorName, &rv.TargetUserID,
		&rv.Rating, &rv.Text, &rv.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, ErrNotFound
	}
	if err != nil {
		return Review{}, fmt.Errorf("get review: %w", err)
	}
	return rv, nil
}

func (r *Repo) ListByTarget(ctx context.Context, targetID uuid.UUID, limit, offset int) ([]Review, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, lead_id, author_user_id, author_name, target_user_id, rating, text, created_at
FROM reviews
WHERE target_user_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3`, targetID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	defer rows.Close()
	out := make([]Review, 0, limit)
	for rows.Next() {
		var rv Review
		if err := rows.Scan(
			&rv.ID, &rv.LeadID, &rv.AuthorUserID, &rv.AuthorName, &rv.TargetUserID,
			&rv.Rating, &rv.Text, &rv.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}
