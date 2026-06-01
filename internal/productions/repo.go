package productions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound       = errors.New("production not found")
	ErrAlreadyExists  = errors.New("active production with this name already exists")
	ErrInUse          = errors.New("production is used by specialists")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

func (r *Repo) Create(ctx context.Context, in CreateInput) (Production, error) {
	var p Production
	err := r.db.QueryRow(ctx,
		`INSERT INTO productions (name, description) VALUES ($1, $2)
		 RETURNING id, name, description, is_active, created_at, updated_at`,
		strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)).
		Scan(&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)
	if isUniqueViolation(err) {
		return Production{}, ErrAlreadyExists
	}
	if err != nil {
		return Production{}, fmt.Errorf("insert production: %w", err)
	}
	return p, nil
}

// List — все продакшены. activeOnly=true исключает деактивированные.
// Используется и админом (видит все) и публичным GET /productions (только активные).
func (r *Repo) List(ctx context.Context, activeOnly bool) ([]Production, error) {
	q := `SELECT id, name, description, is_active, created_at, updated_at FROM productions`
	if activeOnly {
		q += ` WHERE is_active = TRUE`
	}
	q += ` ORDER BY LOWER(name)`
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list productions: %w", err)
	}
	defer rows.Close()
	out := make([]Production, 0)
	for rows.Next() {
		var p Production
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan production: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) Get(ctx context.Context, id uuid.UUID) (Production, error) {
	var p Production
	err := r.db.QueryRow(ctx,
		`SELECT id, name, description, is_active, created_at, updated_at
		 FROM productions WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Production{}, ErrNotFound
	}
	if err != nil {
		return Production{}, fmt.Errorf("get production: %w", err)
	}
	return p, nil
}

// Patch — частичное обновление. Возвращает свежий объект; если ни одно поле
// не передано — просто возвращает текущий. Уникальность имени проверяется
// частичным индексом, при коллизии — ErrAlreadyExists.
func (r *Repo) Patch(ctx context.Context, id uuid.UUID, in PatchInput) (Production, error) {
	sets := make([]string, 0, 3)
	args := []any{id}
	if in.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)+1))
		args = append(args, strings.TrimSpace(*in.Name))
	}
	if in.Description != nil {
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)+1))
		args = append(args, strings.TrimSpace(*in.Description))
	}
	if in.IsActive != nil {
		sets = append(sets, fmt.Sprintf("is_active = $%d", len(args)+1))
		args = append(args, *in.IsActive)
	}
	if len(sets) == 0 {
		return r.Get(ctx, id)
	}
	q := fmt.Sprintf(
		`UPDATE productions SET %s, updated_at = now() WHERE id = $1
		 RETURNING id, name, description, is_active, created_at, updated_at`,
		strings.Join(sets, ", "))
	var p Production
	err := r.db.QueryRow(ctx, q, args...).
		Scan(&p.ID, &p.Name, &p.Description, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Production{}, ErrNotFound
	}
	if isUniqueViolation(err) {
		return Production{}, ErrAlreadyExists
	}
	if err != nil {
		return Production{}, fmt.Errorf("patch production: %w", err)
	}
	return p, nil
}

// Delete — мягкое удаление: ставит is_active=false. Жёстко не удаляем,
// чтобы не разорвать FK у specialist_profiles.production_id (ON DELETE
// SET NULL обнулил бы выбор у живых спецов).
func (r *Repo) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE productions SET is_active = FALSE, updated_at = now() WHERE id = $1`,
		id)
	if err != nil {
		return fmt.Errorf("delete production: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
