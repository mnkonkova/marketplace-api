package invites

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// ErrInvalidInput — пустые/невалидные параметры на входе.
var ErrInvalidInput = errors.New("invalid input")

// DefaultTTL — бриф §11: «TTL инвайта — 7 дней».
const DefaultTTL = 7 * 24 * time.Hour

// EventClientInviteGenerated дублирует константу из projects/events.go,
// чтобы invites не зависел от projects. Webhook-диспатчер (Фаза 6)
// маршрутизирует по event_type, имя совпадает.
const EventClientInviteGenerated = "client_invite.generated"

// AggregateClientInvite — отдельный aggregate для outbox-событий
// генерации инвайта.
const AggregateClientInvite = "client_invite"

// UserDirectory — узкий контракт для получения email/name юзера. Нужен
// чтобы в payload outbox-события (которое отправит n8n с magic-link)
// лежали готовые поля без обратных запросов.
type UserDirectory interface {
	GetEmailAndName(ctx context.Context, userID uuid.UUID) (email, name string, err error)
}

// AppBaseURLer — где хранится публичный URL фронта (нужен для magic-link
// в письме). Тонкая обёртка чтобы не пробрасывать сюда весь config.
type AppBaseURLer interface {
	AppBaseURL() string
}

type Service struct {
	repo    *Repo
	users   UserDirectory
	baseURL string
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) WithUserDirectory(d UserDirectory) *Service {
	s.users = d
	return s
}

// WithAppBaseURL устанавливает базовый URL фронта (https://app.example.com).
// Без него magic-link в payload будет без base URL — n8n должен сам собрать.
func (s *Service) WithAppBaseURL(u string) *Service {
	s.baseURL = u
	return s
}

// ClientInvitePayload — публикуется в outbox при Generate. n8n
// формирует письмо из этих полей.
type ClientInvitePayload struct {
	UserID    uuid.UUID `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name,omitempty"`
	Token     string    `json:"token"`              // raw, для magic-link URL
	BaseURL   string    `json:"base_url,omitempty"` // фронт, чтобы собрать /auth/redeem/{token}
	ExpiresAt time.Time `json:"expires_at"`
}

// Generate выпускает invite и публикует событие в outbox в одной tx.
// CreatedBy — менеджер, который инициировал (или uuid.Nil для service).
func (s *Service) Generate(ctx context.Context, in GenerateInput) (GenerateResult, error) {
	if in.UserID == uuid.Nil {
		return GenerateResult{}, fmt.Errorf("%w: user_id required", ErrInvalidInput)
	}
	if in.TTL == 0 {
		in.TTL = DefaultTTL
	}

	// 1. Генерим инвайт. INSERT в client_invites — это repo.Generate,
	// делает собственный SQL через pool. Outbox-событие пишем СНАРУЖИ
	// этого INSERT'а, но в РАЗНЫХ tx (атомарность best-effort: если
	// outbox упадёт, инвайт уже выпущен в БД, без письма — клиент
	// получит "invitation pending" повтором генерации).
	res, err := s.repo.Generate(ctx, in.UserID, in.CreatedBy, in.TTL)
	if err != nil {
		return GenerateResult{}, err
	}

	// 2. Денормализованные email/name.
	var email, name string
	if s.users != nil {
		email, name, _ = s.users.GetEmailAndName(ctx, in.UserID)
	}

	// 3. Outbox.
	payload := ClientInvitePayload{
		UserID:    in.UserID,
		Email:     email,
		Name:      name,
		Token:     res.RawToken,
		BaseURL:   s.baseURL,
		ExpiresAt: res.ExpiresAt,
	}
	// Отдельная tx для outbox. Падение здесь = инвайт без письма,
	// fixable повтором Generate (новые токены не инвалидируют старые,
	// но клиент получит письмо по новому).
	tx, err := s.repo.db.BeginTx(ctx, pgx.TxOptions{})
	if err == nil {
		_ = outbox.Emit(ctx, tx, AggregateClientInvite, in.UserID.String(),
			EventClientInviteGenerated, payload)
		_ = tx.Commit(ctx)
	}
	return res, nil
}

// Redeem гасит токен и возвращает userID. JWT выпускает auth-сервис
// (этот пакет не должен знать про tokens, чтобы остаться dependency-free).
func (s *Service) Redeem(ctx context.Context, compoundToken string) (uuid.UUID, error) {
	return s.repo.Redeem(ctx, compoundToken)
}
