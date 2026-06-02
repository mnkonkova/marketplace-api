package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrNotFound      = errors.New("user not found")
	ErrNotManager    = errors.New("user is not a manager")
	ErrAlreadyExists = errors.New("user already exists")
	ErrInviteInvalid = errors.New("invite token invalid or expired")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// ListManagers — все пользователи с is_manager=true, отсортированные:
// сначала ожидающие аппрува, потом одобренные. Включает счётчик assigned.
func (r *Repo) ListManagers(ctx context.Context, approved *bool) ([]ManagerInfo, error) {
	q := `
SELECT u.id, COALESCE(u.email::text, ''), u.is_active, u.is_approved,
       u.email_verified_at IS NOT NULL,
       COALESCE(sp.display_name, ''),
       (SELECT COUNT(*) FROM projects p
        WHERE p.assigned_to_user_id = u.id
          AND p.status IN ('draft','active','on_hold','dispute')),
       u.created_at
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE u.is_manager = TRUE`
	args := []any{}
	if approved != nil {
		q += " AND u.is_approved = $1"
		args = append(args, *approved)
	}
	q += " ORDER BY u.is_approved ASC, u.created_at ASC"
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list managers: %w", err)
	}
	defer rows.Close()
	out := make([]ManagerInfo, 0)
	for rows.Next() {
		var m ManagerInfo
		if err := rows.Scan(&m.UserID, &m.Email, &m.IsActive, &m.IsApproved,
			&m.EmailVerified, &m.DisplayName, &m.AssignedProjects, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan manager: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetApproved — менеджеру (только!) меняем is_approved. Если юзер не manager —
// ErrNotManager (защита от случайного одобрения кого попало). Идемпотентен.
func (r *Repo) SetApproved(ctx context.Context, userID uuid.UUID, approved bool) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET is_approved = $2, updated_at = now()
		 WHERE id = $1 AND is_manager = TRUE`,
		userID, approved)
	if err != nil {
		return fmt.Errorf("set approved: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// либо юзер не существует, либо не manager — различим probe-запросом.
		var exists bool
		if perr := r.db.QueryRow(ctx, `SELECT TRUE FROM users WHERE id = $1`, userID).Scan(&exists); perr != nil {
			if errors.Is(perr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("probe user: %w", perr)
		}
		return ErrNotManager
	}
	return nil
}

// CreateClient — заводит юзера (kind=client, без manager/admin-флагов;
// password = случайные 32 байта в base64; юзер всё равно зайдёт по magic-link).
// is_approved=TRUE, email_verified_at=now (раз создаёт админ, верификации не нужно).
func (r *Repo) CreateClient(ctx context.Context, in CreateClientInput) (uuid.UUID, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return uuid.Nil, errors.New("email required")
	}
	// Генерим случайный пароль (юзер не знает — заходит magic-link'ом или
	// сбрасывает). Кладём bcrypt-хеш в users.password_hash, чтобы если
	// password-flow когда-то заработает на этих юзерах, никто не залезет.
	var rnd [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return uuid.Nil, fmt.Errorf("rand: %w", err)
	}
	rawPwd := base64.StdEncoding.EncodeToString(rnd[:])
	hash, err := bcrypt.GenerateFromPassword([]byte(rawPwd), bcrypt.DefaultCost)
	if err != nil {
		return uuid.Nil, fmt.Errorf("bcrypt: %w", err)
	}
	passwordHash := string(hash)

	var userID uuid.UUID
	err = r.db.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, $2, 'client', TRUE, now())
RETURNING id`, email, passwordHash).Scan(&userID)
	if isUniqueViolation(err) {
		return uuid.Nil, ErrAlreadyExists
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create client: %w", err)
	}
	// display_name (если задано) положим в specialist_profiles. Это spec-таблица,
	// но в неё пишет и client — она хранит общий display_name для UI шапки.
	_ = in.DisplayName // оставляем в users.email пока — display_name в Ф6 не критично
	return userID, nil
}

// GenerateInvite — создать magic-link токен для юзера. Гасит предыдущие
// неиспользованные. Возвращает raw-токен (для отправки в письме/в ответе
// админу) и expires_at.
func (r *Repo) GenerateInvite(ctx context.Context, userID, createdBy uuid.UUID, ttl time.Duration) (rawToken string, expiresAt time.Time, err error) {
	// 32 байта → 64 hex символа. Этого с запасом для anti-bruteforce.
	var rnd [32]byte
	if _, e := rand.Read(rnd[:]); e != nil {
		return "", time.Time{}, fmt.Errorf("rand: %w", e)
	}
	rawToken = hex.EncodeToString(rnd[:])
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])
	expiresAt = time.Now().Add(ttl)

	tx, e := r.db.BeginTx(ctx, pgx.TxOptions{})
	if e != nil {
		return "", time.Time{}, fmt.Errorf("begin tx: %w", e)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// существует ли юзер вообще
	var exists bool
	if e := tx.QueryRow(ctx, `SELECT TRUE FROM users WHERE id = $1`, userID).Scan(&exists); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return "", time.Time{}, ErrNotFound
		}
		return "", time.Time{}, fmt.Errorf("check user: %w", e)
	}

	// гасим прошлые неиспользованные
	if _, e := tx.Exec(ctx,
		`UPDATE client_invites SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		userID); e != nil {
		return "", time.Time{}, fmt.Errorf("invalidate prev: %w", e)
	}

	if _, e := tx.Exec(ctx,
		`INSERT INTO client_invites (user_id, token_hash, expires_at, created_by)
		 VALUES ($1, $2, $3, $4)`,
		userID, tokenHash, expiresAt, createdBy); e != nil {
		return "", time.Time{}, fmt.Errorf("insert invite: %w", e)
	}

	if e := tx.Commit(ctx); e != nil {
		return "", time.Time{}, fmt.Errorf("commit: %w", e)
	}
	return rawToken, expiresAt, nil
}

// ConsumeInvite — обмен raw-токена на user_id (для выдачи JWT). Атомарно:
// найти + пометить used + проставить email_verified. Несуществующий /
// использованный / просроченный → ErrInviteInvalid.
func (r *Repo) ConsumeInvite(ctx context.Context, rawToken string) (uuid.UUID, error) {
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID uuid.UUID
	err = tx.QueryRow(ctx, `
UPDATE client_invites
SET used_at = now()
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
RETURNING user_id`, tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInviteInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consume invite: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()),
		                  updated_at = now()
		 WHERE id = $1`, userID); err != nil {
		return uuid.Nil, fmt.Errorf("verify user: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
