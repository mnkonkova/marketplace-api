package auth_test

import (
	"encoding/base64"
	"testing"

	"marketpclce/internal/auth"
)

// HashToken должен быть детерминированным (одинаковый вход → одинаковый
// хэш) и давать 64 hex-символа (sha256). Разные входы — разные хэши.
func TestHashToken(t *testing.T) {
	a := auth.HashToken("abc-123")
	b := auth.HashToken("abc-123")
	if a != b {
		t.Errorf("hash должен быть детерминированным: %q != %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("ожидаем 64 hex-символа (sha256), got %d (%q)", len(a), a)
	}
	if auth.HashToken("abc-123") == auth.HashToken("abc-124") {
		t.Errorf("разные входы → одинаковые хэши: коллизия")
	}
}

// GenerateToken: длина 43 (32 байта в base64.RawURLEncoding), валидный
// base64-URL, статистически уникален.
func TestGenerateToken(t *testing.T) {
	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tok) != 43 {
		t.Errorf("длина токена 43 символа (32 байта base64-RawURL), got %d", len(tok))
	}
	if _, err := base64.RawURLEncoding.DecodeString(tok); err != nil {
		t.Errorf("токен не валидный base64-URL: %v", err)
	}

	// Уникальность 100 итераций. 32 случайных байта → P(коллизия) ≈ 0;
	// фолз позитив здесь — критический баг crypto/rand, что важно ловить.
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		t2, err := auth.GenerateToken()
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if _, dup := seen[t2]; dup {
			t.Fatalf("collision on iter %d: %q", i, t2)
		}
		seen[t2] = struct{}{}
	}
}
