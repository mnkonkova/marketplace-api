package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type TokenKind string

const (
	TokenAccess  TokenKind = "access"
	TokenRefresh TokenKind = "refresh"
	// TokenService — долгоживущий токен для server-to-server вызовов
	// (Directus → наш API). Без exp. Несёт IsService=true и Role
	// (обычно 'admin'). Не выпускается через /auth/* — только CLI
	// (см. cmd/seed --service-token).
	TokenService TokenKind = "service"
)

type Claims struct {
	UserID    uuid.UUID `json:"sub"`
	Kind      TokenKind `json:"kind"`
	Role      string    `json:"role,omitempty"`
	IsService bool      `json:"is_service,omitempty"`
	jwt.RegisteredClaims
}

func (c Claims) GetSubject() (string, error) { return c.UserID.String(), nil }

type TokenIssuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewTokenIssuer(secret string, accessTTL, refreshTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL}
}

type TokenPair struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
}

func (i *TokenIssuer) Issue(userID uuid.UUID, now time.Time) (TokenPair, error) {
	access, err := i.sign(userID, TokenAccess, "", false, now, i.accessTTL)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, err := i.sign(userID, TokenRefresh, "", false, now, i.refreshTTL)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{Access: access, Refresh: refresh}, nil
}

// IssueService выпускает бессрочный service-токен. Используется CLI для
// получения значения DIRECTUS_SERVICE_TOKEN — в рантайме API не выпускает
// service-токены и не принимает запросы на их выдачу.
func (i *TokenIssuer) IssueService(subject uuid.UUID, role string, now time.Time) (string, error) {
	c := Claims{
		UserID:    subject,
		Kind:      TokenService,
		Role:      role,
		IsService: true,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(now),
			// ExpiresAt намеренно пуст: токен бессрочный, ротация — выпуск нового
			// и подмена JWT_SECRET / выдача нового значения в env.
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(i.secret)
}

func (i *TokenIssuer) sign(userID uuid.UUID, kind TokenKind, role string, isService bool, now time.Time, ttl time.Duration) (string, error) {
	c := Claims{
		UserID:    userID,
		Kind:      kind,
		Role:      role,
		IsService: isService,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(i.secret)
}

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrWrongKind    = errors.New("wrong token kind")
)

func (i *TokenIssuer) Parse(token string, expected TokenKind) (Claims, error) {
	c, err := i.parseRaw(token)
	if err != nil {
		return Claims{}, err
	}
	if c.Kind != expected {
		return Claims{}, ErrWrongKind
	}
	return c, nil
}

// ParseAny — Parse с allow-list допустимых kind. Нужен middleware'у,
// принимающему и user access-токен, и service-токен.
func (i *TokenIssuer) ParseAny(token string, allowed ...TokenKind) (Claims, error) {
	c, err := i.parseRaw(token)
	if err != nil {
		return Claims{}, err
	}
	for _, k := range allowed {
		if c.Kind == k {
			return c, nil
		}
	}
	return Claims{}, ErrWrongKind
}

func (i *TokenIssuer) parseRaw(token string) (Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return i.secret, nil
	})
	if err != nil || !parsed.Valid {
		return Claims{}, ErrInvalidToken
	}
	c, ok := parsed.Claims.(*Claims)
	if !ok {
		return Claims{}, ErrInvalidToken
	}
	return *c, nil
}
