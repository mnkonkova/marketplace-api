package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

var ErrInvalidInput = errors.New("invalid input")

type Service struct {
	repo            *Repo
	appBaseURL      string
	inviteTTL       time.Duration
	tokens          *auth.TokenIssuer
}

// NewService — admin-сервис. tokens — для выдачи JWT при redeem_invite.
// appBaseURL — для сборки клиентам ссылки вида {url}/auth/invite?token=.
func NewService(repo *Repo, tokens *auth.TokenIssuer, appBaseURL string, inviteTTL time.Duration) *Service {
	if inviteTTL <= 0 {
		inviteTTL = 7 * 24 * time.Hour // 7 дней по умолчанию
	}
	return &Service{
		repo:       repo,
		appBaseURL: strings.TrimRight(appBaseURL, "/"),
		inviteTTL:  inviteTTL,
		tokens:     tokens,
	}
}

// ---- Менеджеры ----

// ListManagers — approved=nil все, approved=&true только аппрувленные, и т.д.
func (s *Service) ListManagers(ctx context.Context, approved *bool) ([]ManagerInfo, error) {
	return s.repo.ListManagers(ctx, approved)
}

func (s *Service) ApproveManager(ctx context.Context, userID uuid.UUID) error {
	return s.repo.SetApproved(ctx, userID, true)
}

func (s *Service) RevokeManager(ctx context.Context, userID uuid.UUID) error {
	return s.repo.SetApproved(ctx, userID, false)
}

// ---- Клиенты ----

func (s *Service) CreateClient(ctx context.Context, in CreateClientInput, generateInvite bool, createdBy uuid.UUID) (CreateClientResult, error) {
	in.Email = strings.TrimSpace(in.Email)
	if in.Email == "" {
		return CreateClientResult{}, fmt.Errorf("%w: email required", ErrInvalidInput)
	}
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	userID, err := s.repo.CreateClient(ctx, in)
	if err != nil {
		return CreateClientResult{}, err
	}
	res := CreateClientResult{UserID: userID}
	if generateInvite {
		raw, _, err := s.repo.GenerateInvite(ctx, userID, createdBy, s.inviteTTL)
		if err != nil {
			return res, fmt.Errorf("generate invite: %w", err)
		}
		res.InviteToken = raw
		res.InviteURL = s.appBaseURL + "/auth/invite?token=" + raw
	}
	return res, nil
}

func (s *Service) GenerateInvite(ctx context.Context, userID, createdBy uuid.UUID) (InviteGenerateResult, error) {
	raw, expiresAt, err := s.repo.GenerateInvite(ctx, userID, createdBy, s.inviteTTL)
	if err != nil {
		return InviteGenerateResult{}, err
	}
	return InviteGenerateResult{
		Token:     raw,
		URL:       s.appBaseURL + "/auth/invite?token=" + raw,
		ExpiresAt: expiresAt,
	}, nil
}

// RedeemInvite — публичный эндпоинт. Обменивает токен на пару tokens
// (фронт сразу логинит юзера). Юзер становится email_verified.
func (s *Service) RedeemInvite(ctx context.Context, rawToken string) (auth.TokenPair, uuid.UUID, error) {
	if strings.TrimSpace(rawToken) == "" {
		return auth.TokenPair{}, uuid.Nil, fmt.Errorf("%w: token required", ErrInvalidInput)
	}
	userID, err := s.repo.ConsumeInvite(ctx, rawToken)
	if err != nil {
		return auth.TokenPair{}, uuid.Nil, err
	}
	pair, err := s.tokens.Issue(userID, time.Now())
	if err != nil {
		return auth.TokenPair{}, uuid.Nil, fmt.Errorf("issue tokens: %w", err)
	}
	return pair, userID, nil
}
