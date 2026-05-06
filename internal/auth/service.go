package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrBadCredentials = errors.New("bad credentials")
	ErrInvalidInput   = errors.New("invalid input")
	ErrInactive       = errors.New("user is inactive")
)

const (
	KindClient     = "client"
	KindSpecialist = "specialist"
	KindBoth       = "both"
)

type Service struct {
	repo   *Repo
	tokens *TokenIssuer
	now    func() time.Time
}

func NewService(repo *Repo, tokens *TokenIssuer) *Service {
	return &Service{repo: repo, tokens: tokens, now: time.Now}
}

type RegisterInput struct {
	Email       string
	Phone       string
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
	in.Phone = strings.TrimSpace(in.Phone)
	in.DisplayName = strings.TrimSpace(in.DisplayName)

	if in.Email == "" && in.Phone == "" {
		return RegisterResult{}, fmt.Errorf("%w: email or phone is required", ErrInvalidInput)
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
	}
	if in.Email != "" {
		e := strings.ToLower(in.Email)
		user.Email = &e
	}
	if in.Phone != "" {
		user.Phone = &in.Phone
	}

	var userID uuid.UUID
	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		id, err := s.repo.CreateUser(ctx, tx, user)
		if err != nil {
			return err
		}
		userID = id
		if needsProfile {
			// Контакты для заявок предзаполняем тем, что юзер дал при
			// регистрации (email/phone). Auth-поля users.email/users.phone
			// нужны для логина, contact_* — для отображения в брифах;
			// специалист сможет подменить их в /me не ломая логин.
			var profileEmail, profilePhone string
			if user.Email != nil {
				profileEmail = *user.Email
			}
			if user.Phone != nil {
				profilePhone = *user.Phone
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO specialist_profiles (user_id, display_name, contact_email, contact_phone)
				 VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))`,
				id, in.DisplayName, profileEmail, profilePhone); err != nil {
				return fmt.Errorf("insert profile: %w", err)
			}
		}
		return nil
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
