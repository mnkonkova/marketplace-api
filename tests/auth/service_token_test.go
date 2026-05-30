package auth_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

// Service-токен должен:
//   - roundtrip через IssueService/ParseAny с Kind=service;
//   - нести role и is_service=true;
//   - не приниматься Parse(TokenAccess) — это анти-регрессия, чтобы случайный
//     service-токен не прошёл в /me/* как обычный access-токен;
//   - не иметь ExpiresAt (бессрочный) — Parse не должен жаловаться.
func TestIssueAndParseServiceToken(t *testing.T) {
	issuer := auth.NewTokenIssuer("test-secret-12345", time.Minute, time.Hour)
	sub := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	now := time.Now()

	tok, err := issuer.IssueService(sub, "admin", now)
	if err != nil {
		t.Fatalf("IssueService: %v", err)
	}
	if tok == "" {
		t.Fatal("empty service token")
	}

	c, err := issuer.ParseAny(tok, auth.TokenAccess, auth.TokenService)
	if err != nil {
		t.Fatalf("ParseAny: %v", err)
	}
	if c.Kind != auth.TokenService {
		t.Errorf("want kind=service, got %q", c.Kind)
	}
	if !c.IsService {
		t.Error("expected IsService=true")
	}
	if c.Role != "admin" {
		t.Errorf("expected role=admin, got %q", c.Role)
	}
	if c.UserID != sub {
		t.Errorf("subject mismatch: want %v, got %v", sub, c.UserID)
	}
	if c.ExpiresAt != nil {
		t.Errorf("service token must not have ExpiresAt, got %v", c.ExpiresAt)
	}
}

func TestServiceTokenRejectedByAccessParse(t *testing.T) {
	issuer := auth.NewTokenIssuer("test-secret-12345", time.Minute, time.Hour)
	tok, err := issuer.IssueService(uuid.New(), "admin", time.Now())
	if err != nil {
		t.Fatalf("IssueService: %v", err)
	}
	if _, err := issuer.Parse(tok, auth.TokenAccess); err == nil {
		t.Error("service-токен не должен проходить Parse(TokenAccess)")
	}
}

// ParseAny должен отбрасывать токены не из allow-list.
func TestParseAnyRejectsDisallowedKind(t *testing.T) {
	issuer := auth.NewTokenIssuer("test-secret-12345", time.Minute, time.Hour)
	pair, err := issuer.Issue(uuid.New(), time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// refresh-токен в allow-list только access — должен быть отвергнут.
	if _, err := issuer.ParseAny(pair.Refresh, auth.TokenAccess); err == nil {
		t.Error("refresh-токен не должен пройти ParseAny(TokenAccess)")
	}
	// А свой kind — ок.
	if _, err := issuer.ParseAny(pair.Access, auth.TokenAccess, auth.TokenService); err != nil {
		t.Errorf("access-токен должен пройти ParseAny: %v", err)
	}
}

// Регрессия: подпись чужим секретом не должна проходить.
func TestServiceTokenRejectedByWrongSecret(t *testing.T) {
	issA := auth.NewTokenIssuer("secret-A", time.Minute, time.Hour)
	issB := auth.NewTokenIssuer("secret-B", time.Minute, time.Hour)

	tok, err := issA.IssueService(uuid.New(), "admin", time.Now())
	if err != nil {
		t.Fatalf("IssueService: %v", err)
	}
	if _, err := issB.ParseAny(tok, auth.TokenService); err == nil {
		t.Error("токен от issuer A не должен парситься issuer B (другой секрет)")
	}
}
