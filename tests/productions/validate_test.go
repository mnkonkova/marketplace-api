package productions_test

import (
	"errors"
	"strings"
	"testing"

	"marketpclce/internal/productions"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"ok", "Acme Studio", false},
		{"trim ok", "  Acme  ", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"max length", strings.Repeat("a", 120), false},
		{"too long", strings.Repeat("a", 121), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := productions.ValidateName(tc.input)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr && !errors.Is(err, productions.ErrInvalidInput) {
				t.Fatalf("error must wrap ErrInvalidInput, got %v", err)
			}
		})
	}
}
