package auth

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
	ErrNotFound      = errors.New("user not found")
	ErrAlreadyExists = errors.New("user already exists")
	ErrTokenInvalid  = errors.New("verification token invalid or expired")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

type User struct {
	ID              uuid.UUID
	Email           *string
	Phone           *string
	PasswordHash    string
	Kind            string
	IsActive        bool
	EmailVerifiedAt *time.Time
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
SELECT id, email, phone, password_hash, kind, is_active, email_verified_at
FROM users
WHERE (email = $1 OR phone = $1) AND is_active = TRUE
LIMIT 1`
	var u User
	err := r.db.QueryRow(ctx, q, login).Scan(&u.ID, &u.Email, &u.Phone, &u.PasswordHash, &u.Kind, &u.IsActive, &u.EmailVerifiedAt)
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
SELECT id, email, phone, password_hash, kind, is_active, email_verified_at
FROM users WHERE id = $1`
	var u User
	err := r.db.QueryRow(ctx, q, id).Scan(&u.ID, &u.Email, &u.Phone, &u.PasswordHash, &u.Kind, &u.IsActive, &u.EmailVerifiedAt)
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

// GetRole — текущая роль из users.role. Используется в
// RequireAdminOrModerator: claims access-токена не несут role (могла
// поменяться после выпуска), поэтому источник истины — БД.
func (r *Repo) GetRole(ctx context.Context, userID uuid.UUID) (string, error) {
	var role string
	err := r.db.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get role: %w", err)
	}
	return role, nil
}

// GetDisplayName — display_name из specialist_profiles, если есть. Для
// именования юзера в письме. Для kind=client (без профиля) вернёт "".
func (r *Repo) GetDisplayName(ctx context.Context, userID uuid.UUID) (string, error) {
	var name string
	err := r.db.QueryRow(ctx,
		`SELECT display_name FROM specialist_profiles WHERE user_id = $1`,
		userID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get display name: %w", err)
	}
	return name, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// InvalidatePrevVerificationsInTx — гасит все непогашенные токены этого
// юзера (resend). Идемпотентно: если активных нет — no-op.
func (r *Repo) InvalidatePrevVerificationsInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE email_verifications SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		userID)
	if err != nil {
		return fmt.Errorf("invalidate verifications: %w", err)
	}
	return nil
}

// InsertVerificationInTx — записать новый токен. tokenHash — sha256 от raw,
// сам raw в БД не уезжает (защита от дампа). expiresAt передаём из сервиса.
func (r *Repo) InsertVerificationInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, email string, tokenHash string, expiresAt time.Time) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO email_verifications (token_hash, user_id, email, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		tokenHash, userID, email, expiresAt)
	if err != nil {
		return fmt.Errorf("insert verification: %w", err)
	}
	return nil
}

// ConsumeVerification — атомарно: найти неиспользованный непросроченный
// токен → пометить used → проставить users.email_verified_at, если ещё не
// проставлено и email в записи совпадает с current users.email.
// Возвращает user_id успешно подтверждённого. ErrTokenInvalid если токен
// неизвестен/использован/просрочен/email сменился.
func (r *Repo) ConsumeVerification(ctx context.Context, tokenHash string) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID uuid.UUID
	var verifyEmail string
	err = tx.QueryRow(ctx,
		`UPDATE email_verifications
		 SET used_at = now()
		 WHERE token_hash = $1
		   AND used_at IS NULL
		   AND expires_at > now()
		 RETURNING user_id, email`,
		tokenHash).Scan(&userID, &verifyEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrTokenInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consume token: %w", err)
	}

	// Проверяем что email в токене ещё совпадает с current users.email.
	// Защита: если юзер сменил email между выдачей и подтверждением,
	// старый токен подтверждает «не тот» адрес.
	tag, err := tx.Exec(ctx,
		`UPDATE users
		 SET email_verified_at = COALESCE(email_verified_at, now()),
		     updated_at = now()
		 WHERE id = $1 AND email = $2`,
		userID, verifyEmail)
	if err != nil {
		return uuid.Nil, fmt.Errorf("mark verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, ErrTokenInvalid
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

// FindByEmail — поиск по email (case-insensitive через CITEXT). Используется
// в RequestPasswordReset: не отдаём наружу различие «нет такого юзера» vs
// «есть» (anti-enumeration), но сами должны знать чтобы решить, выпускать
// токен или нет.
func (r *Repo) FindByEmail(ctx context.Context, email string) (User, error) {
	const q = `
SELECT id, email, phone, password_hash, kind, is_active, email_verified_at
FROM users
WHERE email = $1 AND is_active = TRUE
LIMIT 1`
	var u User
	err := r.db.QueryRow(ctx, q, email).Scan(&u.ID, &u.Email, &u.Phone, &u.PasswordHash, &u.Kind, &u.IsActive, &u.EmailVerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by email: %w", err)
	}
	return u, nil
}

// InvalidatePrevPasswordResetTokensInTx — гасит активные reset-токены юзера
// при выпуске нового. Без этого старая ссылка из почты осталась бы валидной
// до expires_at, что плохо если её утянули после того как пользователь
// заказал новую.
func (r *Repo) InvalidatePrevPasswordResetTokensInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE password_reset_tokens SET used_at = now()
		 WHERE user_id = $1 AND used_at IS NULL`,
		userID)
	if err != nil {
		return fmt.Errorf("invalidate password reset tokens: %w", err)
	}
	return nil
}

// InsertPasswordResetTokenInTx — записать хеш токена. raw в БД не уезжает.
func (r *Repo) InsertPasswordResetTokenInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO password_reset_tokens (token_hash, user_id, expires_at)
		 VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("insert password reset token: %w", err)
	}
	return nil
}

// ConsumePasswordResetTokenAndUpdate — атомарно: найти валидный токен →
// пометить used → обновить password_hash юзера. Всё в одной транзакции,
// чтобы между «нашли токен» и «обновили пароль» не могло что-то втиснуться.
// Возвращает user_id для последующей выдачи tokens.
func (r *Repo) ConsumePasswordResetTokenAndUpdate(ctx context.Context, tokenHash, newPasswordHash string) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID uuid.UUID
	err = tx.QueryRow(ctx,
		`UPDATE password_reset_tokens
		 SET used_at = now()
		 WHERE token_hash = $1
		   AND used_at IS NULL
		   AND expires_at > now()
		 RETURNING user_id`,
		tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrTokenInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consume password reset token: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE users
		 SET password_hash = $2, updated_at = now()
		 WHERE id = $1 AND is_active = TRUE`,
		userID, newPasswordHash)
	if err != nil {
		return uuid.Nil, fmt.Errorf("update password hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Юзер был деактивирован между выдачей и подтверждением — токен валидным
		// уже не считаем, пароль не меняем.
		return uuid.Nil, ErrTokenInvalid
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}
