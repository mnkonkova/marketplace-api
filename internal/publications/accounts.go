package publications

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Доступы к аккаунтам бренда: где и под каким логином выходят ролики.
//
// Заполняет менеджер, видит заказчик. Креаторам не отдаются: они
// публикуют со своих аккаунтов, а брендовые — собственность клиента.
//
// Пароль шифруется приложением (AES-256-GCM), ключ живёт в окружении.
// Почему не «просто текстом в базе»: дамп и бэкап уезжают туда, где
// чужим паролям от соцсетей не место. Почему не «ключ в базе рядом»:
// это то же самое, что текстом. Нет ключа — ручки отвечают 501, и это
// честнее тихого хранения в открытую.

// AccountPlatformOther — доступ не к площадке: почта, рекламный кабинет.
const AccountPlatformOther = "other"

const (
	accountTitleMax = 120
	accountLoginMax = 200
	accountNoteMax  = 1000
	accountURLMax   = 2000
	accountPassMax  = 500
	// accountsPerProject — предел на проект. Не про технику: список
	// доступов длиннее двух десятков означает, что его ведут не здесь.
	accountsPerProject = 30
)

var (
	// ErrSecretsDisabled — в окружении нет ключа шифрования, и заводить
	// пароли нельзя. Логины и ссылки при этом работают.
	ErrSecretsDisabled = errors.New("хранение паролей выключено: нет ACCOUNTS_SECRET_KEY")
	// ErrTooManyAccounts — в проекте уже некуда добавлять.
	ErrTooManyAccounts = errors.New("слишком много доступов в проекте")
)

// Account — доступ к аккаунту бренда. Пароль наружу этим типом НЕ
// отдаётся: в списке стоит только признак, что он заведён.
type Account struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Platform  string    `json:"platform"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Login     string    `json:"login"`
	// HasPassword — пароль заведён. Само значение отдаётся отдельной
	// ручкой: пароль не должен ездить в каждом списке проекта.
	HasPassword bool       `json:"has_password"`
	Note        string     `json:"note"`
	SortOrder   int        `json:"sort_order"`
	CreatedBy   *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// AccountInput — что присылает менеджер.
type AccountInput struct {
	Platform string
	Title    string
	URL      string
	Login    string
	// Password: nil — не трогать (при правке), пустая строка — стереть,
	// значение — записать. Три состояния, потому что «поле не прислали»
	// и «поле прислали пустым» — разные намерения.
	Password *string
	Note     string
}

// Secrets — шифрование паролей. Пустой ключ = выключено.
type Secrets struct {
	aead cipher.AEAD
}

// NewSecrets — ключ из env: base64 (std или url), 32 байта после
// декодирования. Пустая строка — шифрование выключено, и это не ошибка
// старта: без паролей продукт работает, с паролями в открытую — нет.
func NewSecrets(key string) (*Secrets, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return &Secrets{}, nil
	}
	raw, err := decodeKey(key)
	if err != nil {
		return nil, fmt.Errorf("ACCOUNTS_SECRET_KEY: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("ACCOUNTS_SECRET_KEY: нужно 32 байта после base64, получено %d", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("ACCOUNTS_SECRET_KEY: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("ACCOUNTS_SECRET_KEY: %w", err)
	}
	return &Secrets{aead: aead}, nil
}

func decodeKey(key string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(key); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(key)
}

// Enabled — можно ли хранить пароли.
func (s *Secrets) Enabled() bool { return s != nil && s.aead != nil }

// Seal — nonce||ciphertext. Nonce случайный на каждую запись: один и тот
// же пароль двух аккаунтов не должен давать одинаковые байты в базе.
func (s *Secrets) Seal(plain string) ([]byte, error) {
	if !s.Enabled() {
		return nil, ErrSecretsDisabled
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Open — расшифровать. Испорченный или чужой шифротекст даёт ошибку, а
// не мусор: GCM проверяет целостность.
func (s *Secrets) Open(enc []byte) (string, error) {
	if !s.Enabled() {
		return "", ErrSecretsDisabled
	}
	n := s.aead.NonceSize()
	if len(enc) < n {
		return "", errors.New("шифротекст короче nonce")
	}
	plain, err := s.aead.Open(nil, enc[:n], enc[n:], nil)
	if err != nil {
		return "", fmt.Errorf("расшифровать не удалось: %w", err)
	}
	return string(plain), nil
}

// ---- сервис ----

func (s *Service) validateAccount(in *AccountInput) error {
	in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
	if in.Platform == "" {
		in.Platform = AccountPlatformOther
	}
	if !IsKnownPlatform(in.Platform) && in.Platform != AccountPlatformOther {
		return fmt.Errorf("%w: неизвестная площадка %q", ErrInvalidInput, in.Platform)
	}
	in.Title = strings.TrimSpace(in.Title)
	in.URL = strings.TrimSpace(in.URL)
	in.Login = strings.TrimSpace(in.Login)
	in.Note = strings.TrimSpace(in.Note)
	switch {
	case utf8.RuneCountInString(in.Title) > accountTitleMax:
		return fmt.Errorf("%w: название доступа длиннее %d символов", ErrInvalidInput, accountTitleMax)
	case len(in.URL) > accountURLMax:
		return fmt.Errorf("%w: ссылка слишком длинная", ErrInvalidInput)
	case utf8.RuneCountInString(in.Login) > accountLoginMax:
		return fmt.Errorf("%w: логин слишком длинный", ErrInvalidInput)
	case utf8.RuneCountInString(in.Note) > accountNoteMax:
		return fmt.Errorf("%w: заметка длиннее %d символов", ErrInvalidInput, accountNoteMax)
	}
	if in.URL != "" && !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		return fmt.Errorf("%w: ссылка должна начинаться с http:// или https://", ErrInvalidInput)
	}
	if in.Login == "" && in.URL == "" && in.Title == "" {
		return fmt.Errorf("%w: доступ без ссылки, логина и названия — пустая строка", ErrInvalidInput)
	}
	if in.Password != nil {
		if utf8.RuneCountInString(*in.Password) > accountPassMax {
			return fmt.Errorf("%w: пароль слишком длинный", ErrInvalidInput)
		}
		if *in.Password != "" && !s.secrets.Enabled() {
			return ErrSecretsDisabled
		}
	}
	return nil
}

// sealPassword — что класть в secret_enc: nil «не трогать», []byte{} —
// стереть (в репозитории это NULL), иначе шифротекст.
func (s *Service) sealPassword(in AccountInput) (enc []byte, clear bool, err error) {
	if in.Password == nil {
		return nil, false, nil
	}
	if *in.Password == "" {
		return nil, true, nil
	}
	enc, err = s.secrets.Seal(*in.Password)
	return enc, false, err
}

// ManagerAddAccount — менеджер заводит доступ.
func (s *Service) ManagerAddAccount(ctx context.Context, projectID, actorID uuid.UUID,
	in AccountInput) (Account, error) {

	if err := s.validateAccount(&in); err != nil {
		return Account{}, err
	}
	enc, _, err := s.sealPassword(in)
	if err != nil {
		return Account{}, err
	}
	return s.repo.AddAccount(ctx, projectID, actorID, in, enc)
}

// ManagerUpdateAccount — правка доступа. Пароль не прислали — остаётся
// прежний: чтобы поправить логин, не нужно знать пароль.
func (s *Service) ManagerUpdateAccount(ctx context.Context, projectID, accountID uuid.UUID,
	in AccountInput) (Account, error) {

	if err := s.validateAccount(&in); err != nil {
		return Account{}, err
	}
	enc, clear, err := s.sealPassword(in)
	if err != nil {
		return Account{}, err
	}
	return s.repo.UpdateAccount(ctx, projectID, accountID, in, enc, clear)
}

// Accounts — список доступов проекта. Без паролей.
func (s *Service) Accounts(ctx context.Context, projectID uuid.UUID) ([]Account, error) {
	return s.repo.Accounts(ctx, projectID)
}

// RevealAccountPassword — показать пароль. Отдельной ручкой намеренно:
// пароль не ездит в списке проекта, его запрашивают явно — и по этому
// запросу видно в логах, кто и когда его брал.
func (s *Service) RevealAccountPassword(ctx context.Context, projectID, accountID uuid.UUID) (string, error) {
	if !s.secrets.Enabled() {
		return "", ErrSecretsDisabled
	}
	enc, err := s.repo.AccountSecret(ctx, projectID, accountID)
	if err != nil {
		return "", err
	}
	if len(enc) == 0 {
		return "", ErrNotFound
	}
	return s.secrets.Open(enc)
}

// RemoveAccount — удалить доступ.
func (s *Service) RemoveAccount(ctx context.Context, projectID, accountID uuid.UUID) error {
	return s.repo.RemoveAccount(ctx, projectID, accountID)
}

// ---- репозиторий ----

const accountCols = `id, project_id, platform, title, url, login,
       (secret_enc IS NOT NULL) AS has_password, note, sort_order,
       created_by, created_at, updated_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.ProjectID, &a.Platform, &a.Title, &a.URL, &a.Login,
		&a.HasPassword, &a.Note, &a.SortOrder, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// Accounts — доступы проекта в порядке, который задал менеджер.
func (r *Repo) Accounts(ctx context.Context, projectID uuid.UUID) ([]Account, error) {
	rows, err := r.db.Query(ctx, `
SELECT `+accountCols+`
FROM project_accounts WHERE project_id = $1
ORDER BY sort_order, created_at`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	out := make([]Account, 0, 8)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddAccount — завести доступ. Порядок — следующим за последним.
func (r *Repo) AddAccount(ctx context.Context, projectID, actorID uuid.UUID,
	in AccountInput, enc []byte) (Account, error) {

	var count int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM project_accounts WHERE project_id = $1`, projectID).Scan(&count); err != nil {
		return Account{}, fmt.Errorf("count accounts: %w", err)
	}
	if count >= accountsPerProject {
		return Account{}, ErrTooManyAccounts
	}

	row := r.db.QueryRow(ctx, `
INSERT INTO project_accounts
    (project_id, platform, title, url, login, secret_enc, note, sort_order, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7,
        COALESCE((SELECT max(sort_order) + 1 FROM project_accounts WHERE project_id = $1), 0), $8)
RETURNING `+accountCols,
		projectID, in.Platform, in.Title, in.URL, in.Login, enc, in.Note, actorID)
	a, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("insert account: %w", err)
	}
	return a, nil
}

// UpdateAccount — правка. clear=true стирает пароль, enc=nil при
// clear=false означает «пароль не трогаем».
func (r *Repo) UpdateAccount(ctx context.Context, projectID, accountID uuid.UUID,
	in AccountInput, enc []byte, clear bool) (Account, error) {

	row := r.db.QueryRow(ctx, `
UPDATE project_accounts SET
    platform = $3,
    title = $4,
    url = $5,
    login = $6,
    note = $7,
    secret_enc = CASE WHEN $8 THEN NULL WHEN $9::bytea IS NOT NULL THEN $9 ELSE secret_enc END,
    updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING `+accountCols,
		projectID, accountID, in.Platform, in.Title, in.URL, in.Login, in.Note, clear, enc)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("update account: %w", err)
	}
	return a, nil
}

// AccountSecret — шифротекст пароля.
func (r *Repo) AccountSecret(ctx context.Context, projectID, accountID uuid.UUID) ([]byte, error) {
	var enc []byte
	err := r.db.QueryRow(ctx,
		`SELECT secret_enc FROM project_accounts WHERE project_id = $1 AND id = $2`,
		projectID, accountID).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read secret: %w", err)
	}
	return enc, nil
}

// RemoveAccount — удалить доступ проекта.
func (r *Repo) RemoveAccount(ctx context.Context, projectID, accountID uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM project_accounts WHERE project_id = $1 AND id = $2`, projectID, accountID)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
