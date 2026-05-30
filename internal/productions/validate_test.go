package productions

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"too short 1 char", "a", false},
		{"min boundary", "ab", true},
		{"normal", "Studio Forge", true},
		{"cyrillic min", "Кф", true},
		{"max boundary 120 runes", strings.Repeat("я", 120), true},
		{"over max 121 runes", strings.Repeat("я", 121), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateName(tc.input)
			if tc.ok && err != nil {
				t.Errorf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestValidateDescription(t *testing.T) {
	if err := validateDescription(""); err != nil {
		t.Errorf("empty description should be allowed, got %v", err)
	}
	if err := validateDescription(strings.Repeat("ю", 1000)); err != nil {
		t.Errorf("1000 runes should fit, got %v", err)
	}
	if err := validateDescription(strings.Repeat("ю", 1001)); err == nil {
		t.Errorf("1001 runes should fail")
	}
}

// normalizeAndValidate должен триммить пробелы у обоих полей и применять оба
// валидатора. Граничные случаи: "  ab  " → "ab" (2 руны, ровно min).
func TestNormalizeAndValidate(t *testing.T) {
	t.Run("trims and accepts", func(t *testing.T) {
		n, d, err := normalizeAndValidate("  Studio Forge  ", "  описание  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != "Studio Forge" {
			t.Errorf("name not trimmed: %q", n)
		}
		if d != "описание" {
			t.Errorf("desc not trimmed: %q", d)
		}
	})
	t.Run("trims into too-short name", func(t *testing.T) {
		_, _, err := normalizeAndValidate("  a  ", "")
		if err == nil {
			t.Errorf("expected error for trimmed single char")
		}
	})
	t.Run("min after trim is exactly 2", func(t *testing.T) {
		n, _, err := normalizeAndValidate("   ab   ", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != "ab" {
			t.Errorf("expected ab, got %q", n)
		}
	})
}
