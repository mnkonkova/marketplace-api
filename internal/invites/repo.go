package invites

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrNotFound — токен не существует или для invite_id нет записи.
	ErrNotFound = errors.New("invite not found")
	// ErrUsed — инвайт уже погашен.
	ErrUsed = errors.New("invite already used")
	// ErrExpired — TTL истёк.
	ErrExpired = errors.New("invite expired")
	// ErrBadToken — формат токена не разбирается (нет точки между id и hash).
	ErrBadToken = errors.New("invalid invite token format")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

func (r *Repo) Pool() *pgxpool.Pool { return r.db }

// rawTokenBytes — длина случайной части токена. 32 байта = 256 бит,
// base64.RawURL = 43 символа. Вместе с invite_id UUID и точкой даёт
// токен ≤ 80 символов — помещается в email/URL без обрезки.
const rawTokenBytes = 32

// Generate создаёт новый инвайт:
//  1. Случайные 32 байта → base64.RawURL.
//  2. bcrypt от raw → token_hash.
//  3. INSERT с пред-сгенерированным invite_id.
//  4. Возвращает compound "invite_id.raw" пользователю.
//
// Compound нужен потому что bcrypt non-deterministic; без invite_id мы
// должны были бы итерировать по ВСЕМ активным invite'ам и compare —
// 100ms × N. Compound даёт O(1) lookup по invite_id + один bcrypt.Compare.
func (r *Repo) Generate(ctx context.Context, userID, createdBy uuid.UUID, ttl time.Duration) (GenerateResult, error) {
	// 1. Случайная часть.
	rawBytes := make([]byte, rawTokenBytes)
	if _, err := rand.Read(rawBytes); err != nil {
		return GenerateResult{}, fmt.Errorf("rand: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(rawBytes)

	// 2. bcrypt.
	hash, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.DefaultCost)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("bcrypt: %w", err)
	}

	// 3. INSERT. invite_id генерируем заранее, чтобы вернуть compound.
	inviteID := uuid.New()
	expiresAt := time.Now().UTC().Add(ttl)

	var cb any
	if createdBy != uuid.Nil {
		cb = createdBy
	}

	const q = `
INSERT INTO client_invites (id, user_id, token_hash, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := r.db.Exec(ctx, q, inviteID, userID, string(hash), expiresAt, cb); err != nil {
		return GenerateResult{}, fmt.Errorf("insert invite: %w", err)
	}

	return GenerateResult{
		InviteID:  inviteID,
		RawToken:  inviteID.String() + "." + raw,
		ExpiresAt: expiresAt,
	}, nil
}

// Redeem пытается погасить инвайт по compound-токену:
//  1. Распарсить "invite_id.raw_token".
//  2. SELECT invite по invite_id под row-lock'ом.
//  3. Если used_at != NULL → ErrUsed.
//  4. Если expires_at < now → ErrExpired.
//  5. bcrypt.CompareHashAndPassword.
//  6. UPDATE used_at = now() + INSIDE-same-tx UPDATE users.email_verified_at.
//
// Возвращает userID для последующей выдачи JWT в auth-сервисе.
// Атомарность: всё в одной tx.
func (r *Repo) Redeem(ctx context.Context, compoundToken string) (uuid.UUID, error) {
	// 1. Парсим compound.
	parts := strings.SplitN(compoundToken, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return uuid.Nil, ErrBadToken
	}
	inviteID, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, ErrBadToken
	}
	raw := parts[1]

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 2. SELECT FOR UPDATE.
	var (
		userID    uuid.UUID
		tokenHash string
		expiresAt time.Time
		usedAt    *time.Time
	)
	err = tx.QueryRow(ctx, `
SELECT user_id, token_hash, expires_at, used_at
FROM client_invites
WHERE id = $1
FOR UPDATE`,
		inviteID,
	).Scan(&userID, &tokenHash, &expiresAt, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock invite: %w", err)
	}

	// 3, 4. used / expired.
	if usedAt != nil {
		return uuid.Nil, ErrUsed
	}
	if time.Now().UTC().After(expiresAt) {
		return uuid.Nil, ErrExpired
	}

	// 5. bcrypt.
	if err := bcrypt.CompareHashAndPassword([]byte(tokenHash), []byte(raw)); err != nil {
		// Возвращаем ErrNotFound, чтобы не подсказывать злоумышленнику,
		// что invite_id-то существует.
		return uuid.Nil, ErrNotFound
	}

	// 6. UPDATE.
	if _, err := tx.Exec(ctx, `UPDATE client_invites SET used_at = now() WHERE id = $1`, inviteID); err != nil {
		return uuid.Nil, fmt.Errorf("mark used: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()), updated_at = now() WHERE id = $1`,
		userID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("verify user: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

// CleanupExpired удаляет инвайты, у которых истёк TTL более чем на
// retention. Запускается админ-командой / cron'ом; на API не висит.
// Returns: количество удалённых строк.
func (r *Repo) CleanupExpired(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM client_invites WHERE expires_at < now() - $1::interval`,
		retention.String(),
	)
	if err != nil {
		return 0, fmt.Errorf("cleanup expired: %w", err)
	}
	return tag.RowsAffected(), nil
}
