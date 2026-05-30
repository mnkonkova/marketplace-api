package productions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("production not found")
	// ErrConflict — PATCH прислал устаревший updated_at.
	ErrConflict = errors.New("production updated_at mismatch")
	// ErrDuplicateName — попытка создать/переименовать продакшен в имя,
	// уже занятое среди активных (case-insensitive).
	ErrDuplicateName = errors.New("production name already exists among active")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// ListActive — публичная выдача. Только is_active=TRUE, отсортированы по имени
// (case-insensitive). Описание не возвращаем — оно прячется за PublicProduction
// на уровне хендлера, но для согласованности селектим всё, что есть.
func (r *Repo) ListActive(ctx context.Context) ([]Production, error) {
	return r.list(ctx, true)
}

// ListAll — для admin-эндпоинта: и активные, и архивные.
func (r *Repo) ListAll(ctx context.Context) ([]Production, error) {
	return r.list(ctx, false)
}

func (r *Repo) list(ctx context.Context, onlyActive bool) ([]Production, error) {
	q := `SELECT id, name, description, is_active, created_at, updated_at
          FROM productions`
	if onlyActive {
		q += ` WHERE is_active = TRUE`
	}
	q += ` ORDER BY LOWER(name)`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query productions: %w", err)
	}
	defer rows.Close()

	out := make([]Production, 0, 16)
	for rows.Next() {
		var p Production
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan production: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) GetByID(ctx context.Context, id uuid.UUID) (Production, error) {
	const q = `SELECT id, name, description, is_active, created_at, updated_at
               FROM productions WHERE id = $1`
	var p Production
	err := r.db.QueryRow(ctx, q, id).Scan(
		&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Production{}, ErrNotFound
	}
	if err != nil {
		return Production{}, fmt.Errorf("query production: %w", err)
	}
	return p, nil
}

// Create вставляет новую запись. Дубль среди активных мапится в ErrDuplicateName
// (полагаемся на partial unique index productions_active_name_idx).
func (r *Repo) Create(ctx context.Context, in CreateInput) (Production, error) {
	const q = `INSERT INTO productions (name, description)
               VALUES ($1, $2)
               RETURNING id, name, description, is_active, created_at, updated_at`
	var p Production
	err := r.db.QueryRow(ctx, q, in.Name, in.Description).Scan(
		&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err, "productions_active_name_idx") {
			return Production{}, ErrDuplicateName
		}
		return Production{}, fmt.Errorf("insert production: %w", err)
	}
	return p, nil
}

// Update применяет частичные изменения с optimistic-lock через updated_at.
// expectedUpdatedAt != nil ⇒ в UPDATE добавляется AND updated_at = $X.
// 0 строк → различаем NotFound vs Conflict через probe (как в leads).
// Дубль имени среди активных → ErrDuplicateName.
func (r *Repo) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Production, error) {
	// COALESCE-стиль: nil-поля оставляем как есть.
	const q = `
UPDATE productions
SET name        = COALESCE($2, name),
    description = COALESCE($3, description),
    is_active   = COALESCE($4, is_active),
    updated_at  = now()
WHERE id = $1
  AND ($5::timestamptz IS NULL OR updated_at = $5)
RETURNING id, name, description, is_active, created_at, updated_at`

	var p Production
	err := r.db.QueryRow(ctx, q,
		id, in.Name, in.Description, in.IsActive, in.UpdatedAt,
	).Scan(&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		if in.UpdatedAt != nil {
			// Различаем 404 vs 409: запись есть, но updated_at устарел.
			var exists bool
			if perr := r.db.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM productions WHERE id = $1)`, id,
			).Scan(&exists); perr != nil {
				return Production{}, fmt.Errorf("probe production: %w", perr)
			}
			if exists {
				return Production{}, ErrConflict
			}
		}
		return Production{}, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err, "productions_active_name_idx") {
			return Production{}, ErrDuplicateName
		}
		return Production{}, fmt.Errorf("update production: %w", err)
	}
	return p, nil
}

// Deactivate — soft-delete. expectedUpdatedAt поддерживается ровно как в Update.
// Идемпотентность: повторный вызов на уже деактивированной записи возвращает её
// без ошибки.
func (r *Repo) Deactivate(ctx context.Context, id uuid.UUID, expectedUpdatedAt *time.Time) (Production, error) {
	false_ := false
	return r.Update(ctx, id, UpdateInput{IsActive: &false_, UpdatedAt: expectedUpdatedAt})
}

// CountActiveByLowerName — для service-валидации дубля до INSERT.
// Excluding позволяет на UPDATE не считать саму редактируемую запись.
func (r *Repo) CountActiveByLowerName(ctx context.Context, name string, excludingID *uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM productions
               WHERE is_active = TRUE
                 AND LOWER(name) = LOWER($1)
                 AND ($2::uuid IS NULL OR id <> $2)`
	var n int
	if err := r.db.QueryRow(ctx, q, name, excludingID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count name: %w", err)
	}
	return n, nil
}

// ExistsActive — лёгкая проверка для profiles.Service (валидация
// production_id при PATCH /me/profile).
func (r *Repo) ExistsActive(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM productions WHERE id = $1 AND is_active = TRUE)`,
		id,
	).Scan(&ok); err != nil {
		return false, fmt.Errorf("exists active: %w", err)
	}
	return ok, nil
}

// isUniqueViolation — проверка по SQLSTATE 23505 и имени констрейнта/индекса.
// Привязка к индексу нужна, чтобы при будущих UNIQUE не путать дубль имени с
// чем-то другим.
func isUniqueViolation(err error, indexName string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == indexName
}
