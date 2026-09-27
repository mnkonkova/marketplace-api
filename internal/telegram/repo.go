package telegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Боты. Третьего, менеджерского, нет и не будет: всё, что адресовано
// менеджеру, уходит в общий чат менеджеров существующим CRM-вебхуком.
// Второй бот означал бы две очереди доставки, два места привязки и
// вопрос «куда смотреть» на каждом дежурстве.
const (
	BotCreator = "creator"
	BotClient  = "client"
)

var (
	// ErrNotFound — привязки или кода нет.
	ErrNotFound = errors.New("telegram link not found")
	// ErrCodeExpired — код привязки протух или уже использован.
	ErrCodeExpired = errors.New("telegram link code expired or used")
	// ErrTaken — этот телеграм уже привязан к ДРУГОМУ человеку.
	// Автоматического переноса нет: слияние аккаунтов — работа админа,
	// и делать её молча, по факту нажатия /start, нельзя.
	ErrTaken = errors.New("telegram account is linked to another user")
	// ErrUnknownBot — бот не из двух наших.
	ErrUnknownBot = errors.New("unknown bot")
)

// CodeTTL — сколько живёт код привязки из кабинета. Пятнадцать минут:
// человек нажимает кнопку и тут же идёт в Telegram; длинный срок
// превращает утёкшую ссылку в ключ от аккаунта.
const CodeTTL = 15 * time.Minute

// Link — привязка человека к боту.
type Link struct {
	UserID     uuid.UUID  `json:"user_id"`
	Bot        string     `json:"bot"`
	TGUserID   int64      `json:"tg_user_id"`
	TGChatID   int64      `json:"tg_chat_id"`
	TGUsername string     `json:"tg_username,omitempty"`
	LinkedAt   time.Time  `json:"linked_at"`
	BlockedAt  *time.Time `json:"blocked_at,omitempty"`
}

// Recipient — кому и куда писать. Отбор получателей отдаёт именно это:
// chat_id для отправки и user_id, чтобы бот знал, чей это разговор.
type Recipient struct {
	UserID   uuid.UUID `json:"user_id"`
	TGChatID int64     `json:"tg_chat_id"`
}

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// KnownBot — бот из двух наших. Проверяем в Go, а не CHECK'ом в базе:
// иначе чужое значение доезжает до неё и возвращается пятисоткой
// вместо внятного отказа.
func KnownBot(bot string) bool { return bot == BotCreator || bot == BotClient }

// hashCode — код храним хешем, как инвайты и ссылки входа: дамп базы
// не должен давать привязать чужой аккаунт.
func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// NewCode — выдать одноразовый код привязки.
//
// Возвращает СЫРОЙ код: он уходит человеку в ссылку `t.me/<bot>?start=`
// и больше нигде не лежит. Старые неиспользованные коды этого человека
// гасим: две живые ссылки на один аккаунт — это две двери, а нужна
// одна.
func (r *Repo) NewCode(ctx context.Context, userID uuid.UUID, bot string, now time.Time) (string, error) {
	if !KnownBot(bot) {
		return "", ErrUnknownBot
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)

	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
UPDATE telegram_link_codes SET used_at = $3
WHERE user_id = $1 AND bot = $2 AND used_at IS NULL`, userID, bot, now); err != nil {
			return fmt.Errorf("expire old codes: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO telegram_link_codes (code_hash, user_id, bot, expires_at)
VALUES ($1, $2, $3, $4)`, hashCode(code), userID, bot, now.Add(CodeTTL)); err != nil {
			return fmt.Errorf("insert code: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// LinkByCode — погасить код и записать привязку.
//
// Всё в одной транзакции: код, погашенный без привязки, — это человек,
// который нажал /start и остался без уведомлений, и понять это по
// логам невозможно.
func (r *Repo) LinkByCode(
	ctx context.Context, bot, code string, tgUserID, tgChatID int64, username string, now time.Time,
) (Link, error) {
	if !KnownBot(bot) {
		return Link{}, ErrUnknownBot
	}
	var link Link
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var (
			userID    uuid.UUID
			expiresAt time.Time
			usedAt    *time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT user_id, expires_at, used_at FROM telegram_link_codes
WHERE code_hash = $1 AND bot = $2 FOR UPDATE`, hashCode(code), bot).
			Scan(&userID, &expiresAt, &usedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load code: %w", err)
		}
		if usedAt != nil || now.After(expiresAt) {
			return ErrCodeExpired
		}

		if err := assertFree(ctx, tx, bot, tgUserID, userID); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE telegram_link_codes SET used_at = $2 WHERE code_hash = $1`,
			hashCode(code), now); err != nil {
			return fmt.Errorf("burn code: %w", err)
		}

		l, err := upsertLink(ctx, tx, userID, bot, tgUserID, tgChatID, username, now)
		if err != nil {
			return err
		}
		link = l
		return rememberTelegramID(ctx, tx, userID, tgUserID)
	})
	return link, err
}

// LinkDirect — привязка без кода: человек пришёл из мини-аппа, где мы
// уже проверили подпись Telegram и знаем, кто он.
func (r *Repo) LinkDirect(
	ctx context.Context, userID uuid.UUID, bot string, tgUserID, tgChatID int64,
	username string, now time.Time,
) (Link, error) {
	if !KnownBot(bot) {
		return Link{}, ErrUnknownBot
	}
	var link Link
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if err := assertFree(ctx, tx, bot, tgUserID, userID); err != nil {
			return err
		}
		l, err := upsertLink(ctx, tx, userID, bot, tgUserID, tgChatID, username, now)
		if err != nil {
			return err
		}
		link = l
		return rememberTelegramID(ctx, tx, userID, tgUserID)
	})
	return link, err
}

// assertFree — этот телеграм не занят другим человеком.
//
// Проверяем явно, а не полагаемся на ON CONFLICT: молчаливое «ничего
// не сделали» выглядит как успешная привязка, и человек ждёт
// уведомлений, которые уйдут другому.
func assertFree(ctx context.Context, tx pgx.Tx, bot string, tgUserID int64, userID uuid.UUID) error {
	var owner uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT user_id FROM telegram_links WHERE bot = $1 AND tg_user_id = $2`,
		bot, tgUserID).Scan(&owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check link owner: %w", err)
	case owner != userID:
		return ErrTaken
	default:
		return nil
	}
}

func upsertLink(
	ctx context.Context, tx pgx.Tx, userID uuid.UUID, bot string,
	tgUserID, tgChatID int64, username string, now time.Time,
) (Link, error) {
	var l Link
	// blocked_at сбрасываем: человек вернулся и снова нажал /start —
	// это и есть разблокировка.
	err := tx.QueryRow(ctx, `
INSERT INTO telegram_links (user_id, bot, tg_user_id, tg_chat_id, tg_username, linked_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (bot, tg_user_id) DO UPDATE
   SET user_id = EXCLUDED.user_id, tg_chat_id = EXCLUDED.tg_chat_id,
       tg_username = EXCLUDED.tg_username, linked_at = EXCLUDED.linked_at,
       blocked_at = NULL
RETURNING user_id, bot, tg_user_id, tg_chat_id, tg_username, linked_at, blocked_at`,
		userID, bot, tgUserID, tgChatID, username, now).
		Scan(&l.UserID, &l.Bot, &l.TGUserID, &l.TGChatID, &l.TGUsername, &l.LinkedAt, &l.BlockedAt)
	if err != nil {
		return Link{}, fmt.Errorf("upsert link: %w", err)
	}
	return l, nil
}

// rememberTelegramID — записать телеграм в users.
//
// Это не доставка, а опознание: по нему человек без почты и телефона
// вообще существует в базе (CHECK users_contact_present). Пишем только
// в пустое: у человека может быть привязан другой телеграм в другом
// боте, и переписывать его молча нельзя.
func rememberTelegramID(ctx context.Context, tx pgx.Tx, userID uuid.UUID, tgUserID int64) error {
	_, err := tx.Exec(ctx, `
UPDATE users SET telegram_user_id = $2, updated_at = now()
WHERE id = $1 AND telegram_user_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM users u2 WHERE u2.telegram_user_id = $2)`,
		userID, tgUserID)
	if err != nil {
		return fmt.Errorf("remember telegram id: %w", err)
	}
	return nil
}

// ByTelegram — кто это. Нужен боту на каждое сообщение: он знает
// tg_user_id и больше ничего.
func (r *Repo) ByTelegram(ctx context.Context, bot string, tgUserID int64) (Link, error) {
	var l Link
	err := r.db.QueryRow(ctx, `
SELECT user_id, bot, tg_user_id, tg_chat_id, tg_username, linked_at, blocked_at
FROM telegram_links WHERE bot = $1 AND tg_user_id = $2`, bot, tgUserID).
		Scan(&l.UserID, &l.Bot, &l.TGUserID, &l.TGChatID, &l.TGUsername, &l.LinkedAt, &l.BlockedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("link by telegram: %w", err)
	}
	return l, nil
}

// ListForUser — привязки человека: обе строки, включая заблокированные.
// Кабинет показывает и их: «вы заблокировали бота» — это ответ на
// вопрос «почему не приходит», а отсутствие строки означало бы «не
// подключали».
func (r *Repo) ListForUser(ctx context.Context, userID uuid.UUID) ([]Link, error) {
	rows, err := r.db.Query(ctx, `
SELECT user_id, bot, tg_user_id, tg_chat_id, tg_username, linked_at, blocked_at
FROM telegram_links WHERE user_id = $1 ORDER BY bot`, userID)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()
	out := make([]Link, 0, 2)
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.UserID, &l.Bot, &l.TGUserID, &l.TGChatID,
			&l.TGUsername, &l.LinkedAt, &l.BlockedAt); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkBlocked — человек заблокировал бота.
//
// Строку не удаляем: удаление выглядит как «никогда не подключал», и
// мы бы позвали его подключиться заново — то есть предложили человеку
// то, от чего он только что отказался.
func (r *Repo) MarkBlocked(ctx context.Context, bot string, tgUserID int64, now time.Time) (Link, error) {
	var l Link
	err := r.db.QueryRow(ctx, `
UPDATE telegram_links SET blocked_at = COALESCE(blocked_at, $3)
WHERE bot = $1 AND tg_user_id = $2
RETURNING user_id, bot, tg_user_id, tg_chat_id, tg_username, linked_at, blocked_at`,
		bot, tgUserID, now).
		Scan(&l.UserID, &l.Bot, &l.TGUserID, &l.TGChatID, &l.TGUsername, &l.LinkedAt, &l.BlockedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("mark blocked: %w", err)
	}
	return l, nil
}

// Unlink — отвязать бота по просьбе человека из кабинета.
func (r *Repo) Unlink(ctx context.Context, userID uuid.UUID, bot string) error {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM telegram_links WHERE user_id = $1 AND bot = $2`, userID, bot)
	if err != nil {
		return fmt.Errorf("unlink: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Recipients — кому из этих людей можно написать в этот бот.
//
// Здесь же дневной потолок: 30 сообщений в сутки на человека,
// считается по общему журналу notification_log. Второй счётчик
// разошёлся бы с ним на первом же перезапуске, а человек, которому за
// день пришло тридцать писем, перестаёт читать тридцать первое — и
// заодно все остальные наши.
func (r *Repo) Recipients(
	ctx context.Context, bot string, userIDs []uuid.UUID, day time.Time, dailyCap int,
) ([]Recipient, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT l.user_id, l.tg_chat_id
FROM telegram_links l
WHERE l.bot = $1 AND l.user_id = ANY($2) AND l.blocked_at IS NULL
  AND (SELECT count(*) FROM notification_log n
        WHERE n.user_id = l.user_id AND n.sent_date = $3::date) < $4`,
		bot, userIDs, day, dailyCap)
	if err != nil {
		return nil, fmt.Errorf("recipients: %w", err)
	}
	defer rows.Close()
	out := make([]Recipient, 0, len(userIDs))
	for rows.Next() {
		var rc Recipient
		if err := rows.Scan(&rc.UserID, &rc.TGChatID); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

func (r *Repo) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
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
