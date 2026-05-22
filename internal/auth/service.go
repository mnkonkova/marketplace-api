package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"marketpclce/internal/outbox"
)

// emailMaxLen — RFC 5321 ограничивает локальную часть 64 символами и
// домен 255, итого практический max 254. Не пускаем выше, чтобы не
// держать в БД нереалистичные значения.
const emailMaxLen = 254

var (
	ErrBadCredentials  = errors.New("bad credentials")
	ErrInvalidInput    = errors.New("invalid input")
	ErrInactive        = errors.New("user is inactive")
	ErrEmailUnverified = errors.New("email is not verified")
	ErrResendCooldown  = errors.New("resend cooldown is active")
)

const (
	KindClient     = "client"
	KindSpecialist = "specialist"
	KindBoth       = "both"
)

type Service struct {
	repo                *Repo
	tokens              *TokenIssuer
	now                 func() time.Time
	verifyTokenTTL      time.Duration
	appBaseURL          string
	resendCooldown      ResendCooldown
	verificationOff     bool // если true — soft-gate выключен, юзеры авто-verified
}

// ResendCooldown — узкий интерфейс под Redis-ограничение «не чаще раза в N
// секунд» по user_id. nil-safe: без подключения cooldown'а ресенд проходит
// без ограничений (как до фичи).
type ResendCooldown interface {
	// Acquire возвращает true если запрос разрешён (и берёт окно). False —
	// cooldown ещё активен.
	Acquire(ctx context.Context, key string) (bool, error)
}

func NewService(repo *Repo, tokens *TokenIssuer) *Service {
	return &Service{
		repo:           repo,
		tokens:         tokens,
		now:            time.Now,
		verifyTokenTTL: 24 * time.Hour,
		appBaseURL:     "http://localhost:5173",
	}
}

// WithEmailVerification конфигурирует параметры подтверждения email:
// TTL токена, базовый URL фронта (для verify-ссылки), cooldown-провайдер,
// флаг выключения soft-gate (для локального запуска без Unisender).
// Вызывается из cmd/api/main.go после загрузки config.
func (s *Service) WithEmailVerification(tokenTTL time.Duration, appBaseURL string, cooldown ResendCooldown, disabled bool) *Service {
	if tokenTTL > 0 {
		s.verifyTokenTTL = tokenTTL
	}
	if appBaseURL != "" {
		s.appBaseURL = strings.TrimRight(appBaseURL, "/")
	}
	s.resendCooldown = cooldown
	s.verificationOff = disabled
	return s
}

type RegisterInput struct {
	Email       string
	Password    string
	Kind        string
	DisplayName string
}

type RegisterResult struct {
	UserID uuid.UUID
	Tokens TokenPair
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	in.Email = strings.TrimSpace(in.Email)
	in.DisplayName = strings.TrimSpace(in.DisplayName)

	email, err := validateEmail(in.Email)
	if err != nil {
		return RegisterResult{}, err
	}
	if utf8.RuneCountInString(in.Password) < 8 {
		return RegisterResult{}, fmt.Errorf("%w: password must be at least 8 characters", ErrInvalidInput)
	}
	switch in.Kind {
	case KindClient, KindSpecialist, KindBoth:
	default:
		return RegisterResult{}, fmt.Errorf("%w: kind must be client, specialist or both", ErrInvalidInput)
	}
	needsProfile := in.Kind == KindSpecialist || in.Kind == KindBoth
	if needsProfile && in.DisplayName == "" {
		return RegisterResult{}, fmt.Errorf("%w: display_name is required for specialists", ErrInvalidInput)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("hash password: %w", err)
	}

	user := User{
		PasswordHash: string(hash),
		Kind:         in.Kind,
		Email:        &email,
	}

	var userID uuid.UUID
	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		id, err := s.repo.CreateUser(ctx, tx, user)
		if err != nil {
			return err
		}
		userID = id
		if needsProfile {
			// Контакт для заявок предзаполняем email'ом из регистрации.
			// Auth-поле users.email нужно для логина, contact_email — для
			// отображения в брифах; специалист сможет подменить его в /me
			// не ломая логин.
			if _, err := tx.Exec(ctx,
				`INSERT INTO specialist_profiles (user_id, display_name, contact_email)
				 VALUES ($1, $2, $3)`,
				id, in.DisplayName, *user.Email); err != nil {
				return fmt.Errorf("insert profile: %w", err)
			}
		}
		// Если soft-gate выключен (локальный запуск без Unisender) — сразу
		// помечаем юзера verified и не плодим outbox-события: писем не будет.
		if s.verificationOff {
			if _, err := tx.Exec(ctx,
				`UPDATE users SET email_verified_at = now() WHERE id = $1`, userID); err != nil {
				return fmt.Errorf("auto-verify: %w", err)
			}
			return nil
		}
		// Письмо подтверждения — токен в БД + outbox-событие в той же tx.
		// Если outbox-воркер упадёт, событие останется в outbox и до-отправится
		// при следующем запуске (at-least-once).
		return s.issueVerifyTokenInTx(ctx, tx, userID, *user.Email, in.DisplayName)
	})
	if err != nil {
		return RegisterResult{}, err
	}

	pair, err := s.tokens.Issue(userID, s.now())
	if err != nil {
		return RegisterResult{}, fmt.Errorf("issue tokens: %w", err)
	}
	return RegisterResult{UserID: userID, Tokens: pair}, nil
}

func (s *Service) Login(ctx context.Context, login, password string) (TokenPair, error) {
	login = strings.TrimSpace(login)
	if login == "" || password == "" {
		return TokenPair{}, ErrBadCredentials
	}
	if strings.Contains(login, "@") {
		login = strings.ToLower(login)
	}
	u, err := s.repo.FindByLogin(ctx, login)
	if errors.Is(err, ErrNotFound) {
		return TokenPair{}, ErrBadCredentials
	}
	if err != nil {
		return TokenPair{}, err
	}
	if !u.IsActive {
		return TokenPair{}, ErrInactive
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return TokenPair{}, ErrBadCredentials
	}
	return s.tokens.Issue(u.ID, s.now())
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	c, err := s.tokens.Parse(refreshToken, TokenRefresh)
	if err != nil {
		return TokenPair{}, ErrInvalidToken
	}
	u, err := s.repo.FindByID(ctx, c.UserID)
	if err != nil {
		return TokenPair{}, ErrInvalidToken
	}
	if !u.IsActive {
		return TokenPair{}, ErrInactive
	}
	return s.tokens.Issue(u.ID, s.now())
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	return s.repo.FindByID(ctx, id)
}

// IsEmailVerified — централизованная проверка для soft-gate (POST /leads,
// POST /me/profile/publish). Контракт:
//   - (true, nil)  — почта подтверждена;
//   - (false, nil) — почта НЕ подтверждена (нормальный кейс soft-gate);
//   - (_, err)     — ошибка БД/чтения, не путать с «не подтверждено».
// Если verification выключен в конфиге — всегда возвращает (true, nil).
func (s *Service) IsEmailVerified(ctx context.Context, id uuid.UUID) (bool, error) {
	if s.verificationOff {
		return true, nil
	}
	u, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return false, err
	}
	return u.EmailVerifiedAt != nil, nil
}

// VerifyEmail — обмен raw-токена из письма на «email подтверждён».
// Возвращает свежую пару tokens чтобы фронт получил access с актуальным
// состоянием (если используется в claims).
// Если verification выключен — токенов в БД нет, возвращаем ErrTokenInvalid
// (фронт это покажет как «ссылка устарела», что honest для dev-режима).
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) (TokenPair, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return TokenPair{}, ErrInvalidInput
	}
	if s.verificationOff {
		return TokenPair{}, ErrTokenInvalid
	}
	userID, err := s.repo.ConsumeVerification(ctx, hashToken(rawToken))
	if err != nil {
		return TokenPair{}, err
	}
	return s.tokens.Issue(userID, s.now())
}

// ResendVerification — генерит новый токен, гасит прошлые, эмитит outbox.
// RL через ResendCooldown (по user_id). Идемпотентно по факту: каждая
// успешная отправка инвалидирует ранее выданные.
// Если verification выключен в конфиге — no-op (возвращает nil): фронт
// получит 204 и баннер не покажется, потому что юзер уже verified.
func (s *Service) ResendVerification(ctx context.Context, userID uuid.UUID) error {
	if s.verificationOff {
		return nil
	}
	u, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.EmailVerifiedAt != nil {
		// Уже подтверждён — нечего ресендить. Возвращаем ничего (idempotent).
		return nil
	}
	if u.Email == nil {
		return ErrInvalidInput
	}
	if s.resendCooldown != nil {
		ok, err := s.resendCooldown.Acquire(ctx, "email-verify-resend:"+userID.String())
		if err != nil {
			// Не валим запрос если Redis недоступен — отправим, но логировать
			// должен caller. На уровне сервиса жёсткой ошибки нет.
			ok = true
		}
		if !ok {
			return ErrResendCooldown
		}
	}
	// display_name тащим из specialist_profiles если есть, чтобы письмо было
	// именным. Для клиента-без-профиля будет ToName="" и mailer вставит generic.
	displayName, _ := s.repo.GetDisplayName(ctx, userID)
	return s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.InvalidatePrevVerificationsInTx(ctx, tx, userID); err != nil {
			return err
		}
		return s.issueVerifyTokenInTx(ctx, tx, userID, *u.Email, displayName)
	})
}

// issueVerifyTokenInTx — общий код для register и resend: генерит токен,
// пишет хэш в БД, эмитит outbox-событие с raw-токеном для письма.
func (s *Service) issueVerifyTokenInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, email, displayName string) error {
	raw, err := generateToken()
	if err != nil {
		return fmt.Errorf("gen token: %w", err)
	}
	expiresAt := s.now().Add(s.verifyTokenTTL)
	if err := s.repo.InsertVerificationInTx(ctx, tx, userID, email, hashToken(raw), expiresAt); err != nil {
		return err
	}
	return outbox.Emit(ctx, tx, outbox.AggregateEmail, userID.String(),
		outbox.EventEmailVerifySend, outbox.EmailVerifyPayload{
			To:      email,
			ToName:  displayName,
			Token:   raw,
			BaseURL: s.appBaseURL,
		})
}

// generateToken — 32 случайных байта в base64 URL-safe (43 символа).
// crypto/rand — заведомо CSPRNG, для одноразовых ссылок подтверждения
// этого с большим запасом достаточно.
func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// hashToken — sha256(raw) → hex. В БД храним хэш, не raw, чтобы дамп БД
// не позволял подтвердить чужую почту.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// validateEmail — RFC 5322 разбор + санитарные проверки.
// Возвращает нормализованный email (lower-case, trimmed) или ErrInvalidInput.
//
// net/mail.ParseAddress принимает форму "Name <addr@host>" — нам нужен только
// addr, иначе пропустим "Foo <a@b>". Поэтому ловим .Address из результата
// и сверяем что во входе не было display-name-обёртки (== addr.Address ровно
// равен input после lower).
func validateEmail(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: email is required", ErrInvalidInput)
	}
	if len(s) > emailMaxLen {
		return "", fmt.Errorf("%w: email too long", ErrInvalidInput)
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("%w: bad email format", ErrInvalidInput)
	}
	// Запрещаем форму "Name <a@b>" — фронт должен слать чистый адрес.
	normalized := strings.ToLower(strings.TrimSpace(addr.Address))
	if normalized != strings.ToLower(s) {
		return "", fmt.Errorf("%w: email must be a plain address, not 'Name <addr>'", ErrInvalidInput)
	}
	if !strings.Contains(normalized, ".") {
		// ParseAddress пропускает "user@localhost". Для маркетплейса требуем
		// домен с точкой — отсекает явные опечатки и тестовые значения.
		return "", fmt.Errorf("%w: email domain must contain a dot", ErrInvalidInput)
	}
	return normalized, nil
}
