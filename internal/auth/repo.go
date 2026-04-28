package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound      = errors.New("user not found")
	ErrAlreadyExists = errors.New("user already exists")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

type User struct {
	ID           uuid.UUID
	Email        *string
	Phone        *string
	PasswordHash string
	Kind         string
	IsActive     bool
}

func (r *Repo) CreateUser(ctx context.Context, tx pgx.Tx, u User) (uuid.UUID, error) {
	const q = `
INSERT INTO users (email, phone, password_hash, kind)
VALUES ($1, $2, $3, $4)
RETURNING id`
	var id uuid.UUID
	err := tx.QueryRow(ctx, q, u.Email, u.Phone, u.PasswordHash, u.Kind).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, ErrAlreadyExists
		}
		return uuid.Nil, fmt.Errorf("insert user: %w", err)
	}
	return id, nil
}

func (r *Repo) FindByLogin(ctx context.Context, login string) (User, error) {
	const q = `
SELECT id, email, phone, password_hash, kind, is_active
FROM users
WHERE (email = $1 OR phone = $1) AND is_active = TRUE
LIMIT 1`
	var u User
	err := r.db.QueryRow(ctx, q, login).Scan(&u.ID, &u.Email, &u.Phone, &u.PasswordHash, &u.Kind, &u.IsActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user: %w", err)
	}
	return u, nil
}

func (r *Repo) FindByID(ctx context.Context, id uuid.UUID) (User, error) {
	const q = `
SELECT id, email, phone, password_hash, kind, is_active
FROM users WHERE id = $1`
	var u User
	err := r.db.QueryRow(ctx, q, id).Scan(&u.ID, &u.Email, &u.Phone, &u.PasswordHash, &u.Kind, &u.IsActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user: %w", err)
	}
	return u, nil
}

func (r *Repo) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
