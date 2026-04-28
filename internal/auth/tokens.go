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
)

type Claims struct {
	UserID uuid.UUID `json:"sub"`
	Kind   TokenKind `json:"kind"`
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
	access, err := i.sign(userID, TokenAccess, now, i.accessTTL)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, err := i.sign(userID, TokenRefresh, now, i.refreshTTL)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{Access: access, Refresh: refresh}, nil
}

func (i *TokenIssuer) sign(userID uuid.UUID, kind TokenKind, now time.Time, ttl time.Duration) (string, error) {
	c := Claims{
		UserID: userID,
		Kind:   kind,
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
	if c.Kind != expected {
		return Claims{}, ErrWrongKind
	}
	return *c, nil
}
