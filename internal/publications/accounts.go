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
	"net/url"
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
	// ErrAccountExists — у этого креатора уже заведён аккаунт этой
	// площадки в этом проекте.
	ErrAccountExists = errors.New("у креатора уже есть аккаунт этой площадки")
)

// Account — доступ к аккаунту бренда. Пароль наружу этим типом НЕ
// отдаётся: в списке стоит только признак, что он заведён.
type Account struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	// CreatorUserID — чей это аккаунт. Ролики выходят С АККАУНТОВ
	// КРЕАТОРОВ, и список без владельца читается как чужая связка
	// ключей: пять строк, и неясно, с кого спрашивать, когда ссылка
	// перестала отвечать. Пусто у настоящих брендовых доступов —
	// почты, рекламного кабинета, аккаунта самого бренда.
	CreatorUserID *uuid.UUID `json:"creator_user_id,omitempty"`
	// CreatorName — подпись владельца. Считается в запросе, в таблице
	// доступов её нет.
	CreatorName string `json:"creator_name,omitempty"`
	Platform    string `json:"platform"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Login       string `json:"login"`
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
	// CreatorUserID — чей аккаунт. nil оставляет доступ без владельца:
	// так заводят брендовые. Проверку «этот человек в составе проекта»
	// делает репозиторий — на этом же запросе, а не вторым.
	CreatorUserID *uuid.UUID
	Platform      string
	Title         string
	URL           string
	Login         string
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
	if in.URL != "" {
		normalized, err := normalizeAccountURL(in.URL)
		if err != nil {
			return err
		}
		in.URL = normalized
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

// normalizeAccountURL — привести адрес аккаунта к виду, по которому по
// нему можно кликнуть.
//
// Схему дописываем, а не требуем. Ссылку на аккаунт копируют из адресной
// строки и из шапки приложения, и там она сплошь и рядом без «https://»:
// «tiktok.com/@nastya». Отказ на этом месте выглядел как «ссылка
// неправильная», хотя она правильная — и ровно её же ParseLink принимает
// при сдаче ролика («Креатор копирует ссылку из приложения и часто без
// схемы»). Две разные строгости к одному и тому же адресу в одном
// продукте — это не защита, а ловушка.
//
// Чужие схемы отклоняем по-прежнему, и это не формальность: адрес уходит
// в href, а javascript: и data: там — исполняемый код на странице
// заказчика.
func normalizeAccountURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") {
		scheme := strings.ToLower(s[:strings.Index(s, "://")])
		if scheme != "http" && scheme != "https" {
			return "", fmt.Errorf("%w: ссылка должна быть http или https", ErrInvalidInput)
		}
	} else {
		// Встречается и «javascript:alert(1)» без слэшей — двоеточие в
		// начале строки схемой и является.
		if i := strings.Index(s, ":"); i > 0 && !strings.ContainsAny(s[:i], "./ ") {
			return "", fmt.Errorf("%w: ссылка должна быть http или https", ErrInvalidInput)
		}
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, ".") {
		return "", fmt.Errorf("%w: это не похоже на ссылку на аккаунт", ErrInvalidInput)
	}
	return s, nil
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

// Имя владельца берём тем же запросом: отдельный проход по составу
// проекта ради подписи в списке — лишний round-trip на каждом открытии
// карточки.
const accountCols = `a.id, a.project_id, a.creator_user_id,
       COALESCE(sp.display_name, cp.display_name, split_part(u.email, '@', 1), '') AS creator_name,
       a.platform, a.title, a.url, a.login,
       (a.secret_enc IS NOT NULL) AS has_password, a.note, a.sort_order,
       a.created_by, a.created_at, a.updated_at`

// accountJoins — откуда берётся имя владельца. LEFT: владельца может не
// быть вовсе, а у него может не быть ни профиля, ни имени.
const accountJoins = `LEFT JOIN users u ON u.id = a.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = a.creator_user_id
LEFT JOIN client_profiles cp ON cp.user_id = a.creator_user_id`

// accountFrom — источник для accountCols при чтении.
const accountFrom = `FROM project_accounts a ` + accountJoins

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.ProjectID, &a.CreatorUserID, &a.CreatorName,
		&a.Platform, &a.Title, &a.URL, &a.Login,
		&a.HasPassword, &a.Note, &a.SortOrder, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// Accounts — доступы проекта в порядке, который задал менеджер.
func (r *Repo) Accounts(ctx context.Context, projectID uuid.UUID) ([]Account, error) {
	rows, err := r.db.Query(ctx, `
SELECT `+accountCols+`
`+accountFrom+`
WHERE a.project_id = $1
ORDER BY a.sort_order, a.created_at`, projectID)
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

	// Владелец — только из действующего состава проекта: доступ,
	// подписанный человеком, которого в проекте нет, врёт о том, с кого
	// спрашивать.
	if in.CreatorUserID != nil {
		var inProject bool
		if err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_creators
    WHERE project_id = $1 AND creator_user_id = $2 AND removed_at IS NULL
)`, projectID, *in.CreatorUserID).Scan(&inProject); err != nil {
			return Account{}, fmt.Errorf("check creator: %w", err)
		}
		if !inProject {
			return Account{}, ErrCreatorNotInProject
		}
	}

	// Читаем из самой CTE, а не из таблицы: строку, вставленную
	// data-modifying CTE, обычный SELECT в том же запросе не видит —
	// он работает со снимком, сделанным до вставки. Первый вариант
	// делал именно так и падал «no rows in result set» на каждом
	// добавлении.
	row := r.db.QueryRow(ctx, `
WITH a AS (
    INSERT INTO project_accounts
        (project_id, creator_user_id, platform, title, url, login, secret_enc, note, sort_order, created_by)
    VALUES ($1, $9, $2, $3, $4, $5, $6, $7,
            COALESCE((SELECT max(sort_order) + 1 FROM project_accounts WHERE project_id = $1), 0), $8)
    RETURNING *
)
SELECT `+accountCols+`
FROM a `+accountJoins,
		projectID, in.Platform, in.Title, in.URL, in.Login, enc, in.Note, actorID, in.CreatorUserID)
	a, err := scanAccount(row)
	if isUniqueViolation(err) {
		// project_accounts_creator_platform_uniq: у человека уже есть
		// аккаунт этой площадки в проекте. Второй — это не второй
		// аккаунт, а тот же, заведённый дважды.
		return Account{}, ErrAccountExists
	}
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
WITH a AS (
    UPDATE project_accounts SET
        creator_user_id = $10,
        platform = $3,
        title = $4,
        url = $5,
        login = $6,
        note = $7,
        secret_enc = CASE WHEN $8 THEN NULL WHEN $9::bytea IS NOT NULL THEN $9 ELSE secret_enc END,
        updated_at = now()
    WHERE project_id = $1 AND id = $2
    RETURNING *
)
SELECT `+accountCols+`
FROM a `+accountJoins,
		projectID, accountID, in.Platform, in.Title, in.URL, in.Login, in.Note, clear, enc,
		in.CreatorUserID)
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

// ---- аккаунты глазами креатора ----
//
// «Мои аккаунты» на странице проекта — это аккаунты ЭТОГО проекта, а не
// личная страница из профиля специалиста. Креатор заводит под проект
// отдельные, и ведёт их он сам: менеджер не знает, с какого аккаунта
// человек решил выкладывать, и переписывание этого через чат — ровно тот
// шаг, ради устранения которого блок и существует.
//
// Границы у креаторского пути те же, что у правки ссылки: он видит и
// правит ТОЛЬКО свои строки. Брендовые доступы (creator_user_id IS NULL)
// и чужие — не его дело: в них лежат пароли заказчика.

// CreatorAccounts — аккаунты этого креатора в этом проекте.
func (s *Service) CreatorAccounts(ctx context.Context, projectID, creatorID uuid.UUID) ([]Account, error) {
	return s.repo.CreatorAccounts(ctx, projectID, creatorID)
}

// CreatorAddAccount — креатор заводит свой аккаунт в проекте.
//
// Владельца не спрашиваем, а проставляем: чужую строку под видом своей
// завести нельзя.
func (s *Service) CreatorAddAccount(ctx context.Context, projectID, creatorID uuid.UUID,
	in AccountInput) (Account, error) {

	in.CreatorUserID = &creatorID
	if err := s.validateAccount(&in); err != nil {
		return Account{}, err
	}
	enc, _, err := s.sealPassword(in)
	if err != nil {
		return Account{}, err
	}
	return s.repo.AddAccount(ctx, projectID, creatorID, in, enc)
}

// CreatorUpdateAccount — правка своего аккаунта.
func (s *Service) CreatorUpdateAccount(ctx context.Context, projectID, accountID, creatorID uuid.UUID,
	in AccountInput) (Account, error) {

	if err := s.assertOwnAccount(ctx, projectID, accountID, creatorID); err != nil {
		return Account{}, err
	}
	in.CreatorUserID = &creatorID
	if err := s.validateAccount(&in); err != nil {
		return Account{}, err
	}
	enc, clear, err := s.sealPassword(in)
	if err != nil {
		return Account{}, err
	}
	return s.repo.UpdateAccount(ctx, projectID, accountID, in, enc, clear)
}

// CreatorRemoveAccount — снять свой аккаунт с проекта.
func (s *Service) CreatorRemoveAccount(ctx context.Context, projectID, accountID, creatorID uuid.UUID) error {
	if err := s.assertOwnAccount(ctx, projectID, accountID, creatorID); err != nil {
		return err
	}
	return s.repo.RemoveAccount(ctx, projectID, accountID)
}

// CreatorRevealAccountPassword — показать пароль своего аккаунта.
//
// Свой пароль креатор забывает так же, как все: он его и вписывал. Чужой
// не покажем — проверка владельца стоит до запроса шифротекста.
func (s *Service) CreatorRevealAccountPassword(ctx context.Context,
	projectID, accountID, creatorID uuid.UUID) (string, error) {

	if err := s.assertOwnAccount(ctx, projectID, accountID, creatorID); err != nil {
		return "", err
	}
	return s.RevealAccountPassword(ctx, projectID, accountID)
}

// assertOwnAccount — строка принадлежит этому креатору.
//
// Чужая и несуществующая отвечают ОДИНАКОВО: «нет такой». Разные ответы
// подтвердили бы постороннему, что доступ существует, — а в доступах
// лежат пароли.
func (s *Service) assertOwnAccount(ctx context.Context, projectID, accountID, creatorID uuid.UUID) error {
	mine, err := s.repo.AccountBelongsTo(ctx, projectID, accountID, creatorID)
	if err != nil {
		return err
	}
	if !mine {
		return ErrNotFound
	}
	return nil
}

// CreatorAccounts — только свои строки. Брендовые (без владельца) сюда
// не попадают намеренно: в них пароли заказчика.
func (r *Repo) CreatorAccounts(ctx context.Context, projectID, creatorID uuid.UUID) ([]Account, error) {
	rows, err := r.db.Query(ctx, `SELECT `+accountCols+` `+accountFrom+`
WHERE a.project_id = $1 AND a.creator_user_id = $2
ORDER BY a.sort_order, a.created_at`, projectID, creatorID)
	if err != nil {
		return nil, fmt.Errorf("list creator accounts: %w", err)
	}
	defer rows.Close()
	out := make([]Account, 0)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan creator account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountBelongsTo — его ли это доступ.
func (r *Repo) AccountBelongsTo(ctx context.Context, projectID, accountID, creatorID uuid.UUID) (bool, error) {
	var mine bool
	if err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_accounts
    WHERE project_id = $1 AND id = $2 AND creator_user_id = $3
)`, projectID, accountID, creatorID).Scan(&mine); err != nil {
		return false, fmt.Errorf("check account owner: %w", err)
	}
	return mine, nil
}
