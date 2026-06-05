package reviews

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
)

// isUniqueViolation — PG SQLSTATE 23505. Используется для перевода
// дубликата UNIQUE-индекса в ErrDuplicate (data-sec D10).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

var (
	ErrNotFound  = errors.New("review not found")
	ErrForbidden = errors.New("not the author")
	ErrLeadCheck = errors.New("lead does not authorize this review")
	// ErrTargetNotSpecialist — target_user_id принадлежит юзеру без
	// specialist_profile. Раньше отдавалось как ErrInvalidInput из сервиса,
	// теперь проверка перенесена в tx (data-sec D9), отдельный sentinel
	// чтобы хендлер мог различить 400 vs 500.
	ErrTargetNotSpecialist = errors.New("target is not a specialist")
	// ErrDuplicate — UNIQUE-нарушение по (lead_id, author, target) ИЛИ
	// по партиальному UNIQUE (author, target) WHERE lead_id IS NULL
	// (миграция 00017, data-sec D10): один автор не может оставить
	// несколько отзывов одному спецу без привязки к лиду.
	ErrDuplicate = errors.New("review already exists for this pair")
	// ErrConflict — клиент прислал UpdateInput.UpdatedAt не совпадающий
	// с текущим updated_at (кто-то параллельно отредактировал отзыв).
	ErrConflict = errors.New("review updated_at mismatch")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// TargetIsSpecialist / LeadAuthorizesReview раньше были публичными
// методами, которые сервис дёргал вне tx. Это давало TOCTOU-окно
// (data-sec D9), поэтому проверки переехали внутрь Create-tx ниже.
// Отдельные функции удалены — единая точка контроля.

func (r *Repo) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// data-sec D9: проверки target-роли и lead-авторизации идут ВНУТРИ
	// той же tx, что и INSERT. FOR SHARE на specialist_profiles фиксирует
	// строку — между check и insert никто не удалит профиль/не снимет
	// аксепт лида. Раньше эти проверки делались в Service вне tx, и
	// между ними и INSERT было окно для гонки.
	var isSpec bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM specialist_profiles WHERE user_id = $1 FOR SHARE)`,
		in.TargetUserID).Scan(&isSpec); err != nil {
		return uuid.Nil, fmt.Errorf("check target: %w", err)
	}
	if !isSpec {
		return uuid.Nil, ErrTargetNotSpecialist
	}
	if in.LeadID != nil {
		var leadOK bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1
  FROM leads l
  JOIN lead_recipients lr ON lr.lead_id = l.id
  WHERE l.id = $1
    AND l.client_user_id = $2
    AND lr.specialist_user_id = $3
    AND lr.status = 'accepted'
  FOR SHARE OF lr
)`, *in.LeadID, in.AuthorUserID, in.TargetUserID).Scan(&leadOK); err != nil {
			return uuid.Nil, fmt.Errorf("check lead: %w", err)
		}
		if !leadOK {
			return uuid.Nil, ErrLeadCheck
		}
	}

	// data-sec D7: имя автора резолвим из БД, не из тела запроса.
	// Фолбэк-ладдер тот же, что у LoadClientDisplayNames (projects/manager_repo):
	// specialist_profiles → client_profiles → префикс email.
	var authorName string
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(
  NULLIF(sp.display_name, ''),
  NULLIF(cp.display_name, ''),
  split_part(u.email, '@', 1),
  ''
)
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles      cp ON cp.user_id = u.id
WHERE u.id = $1`, in.AuthorUserID).Scan(&authorName); err != nil {
		return uuid.Nil, fmt.Errorf("resolve author name: %w", err)
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
INSERT INTO reviews (lead_id, author_user_id, author_name, target_user_id, rating, text)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`,
		in.LeadID, in.AuthorUserID, authorName, in.TargetUserID, in.Rating, in.Text,
	).Scan(&id)
	if isUniqueViolation(err) {
		// Уник-нарушение по одному из двух индексов:
		// (lead_id, author, target) или (author, target) WHERE lead_id IS NULL.
		return uuid.Nil, ErrDuplicate
	}
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
  rating     = COALESCE($3, rating),
  text       = COALESCE($4, text),
  updated_at = now()
WHERE id = $1 AND author_user_id = $2
  AND ($5::timestamptz IS NULL OR updated_at = $5)
RETURNING target_user_id`,
		id, authorID, in.Rating, in.Text, in.UpdatedAt,
	).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		// 0 строк: 3 варианта — нет отзыва (404), чужой автор (403),
		// устаревший updated_at (409). Различаем доп. запросом.
		var rowExists, authorMatches bool
		if err := r.db.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM reviews WHERE id = $1),
       EXISTS(SELECT 1 FROM reviews WHERE id = $1 AND author_user_id = $2)`,
			id, authorID).Scan(&rowExists, &authorMatches); err != nil {
			return uuid.Nil, fmt.Errorf("probe review: %w", err)
		}
		if !rowExists {
			return uuid.Nil, ErrNotFound
		}
		if !authorMatches {
			return uuid.Nil, ErrForbidden
		}
		// Запись есть, автор тот — значит провалилась optimistic-проверка.
		return uuid.Nil, ErrConflict
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
SELECT id, lead_id, author_user_id, author_name, target_user_id, rating, text, created_at, updated_at
FROM reviews WHERE id = $1`, id).Scan(
		&rv.ID, &rv.LeadID, &rv.AuthorUserID, &rv.AuthorName, &rv.TargetUserID,
		&rv.Rating, &rv.Text, &rv.CreatedAt, &rv.UpdatedAt,
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
SELECT id, lead_id, author_user_id, author_name, target_user_id, rating, text, created_at, updated_at
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
			&rv.Rating, &rv.Text, &rv.CreatedAt, &rv.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}
