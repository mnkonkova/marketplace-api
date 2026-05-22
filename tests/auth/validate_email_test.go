package auth_test

import (
	"errors"
	"strings"
	"testing"

	"marketpclce/internal/auth"
)

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // нормализованный email; "" если ожидаем ошибку
		wantErr bool
	}{
		// happy paths
		{name: "plain lowercase", in: "user@example.com", want: "user@example.com"},
		{name: "mixed case → lower", in: "USER@Example.COM", want: "user@example.com"},
		{name: "with subdomain", in: "u@sub.example.com", want: "u@sub.example.com"},
		{name: "with plus tag", in: "user+tag@example.com", want: "user+tag@example.com"},
		{name: "with dots in local", in: "first.last@example.com", want: "first.last@example.com"},

		// error paths
		{name: "empty", in: "", wantErr: true},
		{name: "no at", in: "userexample.com", wantErr: true},
		{name: "domain without dot", in: "user@localhost", wantErr: true},
		{name: "with display name", in: "Foo <foo@example.com>", wantErr: true},
		{name: "too long", in: strings.Repeat("a", 250) + "@x.com", wantErr: true},
		{name: "whitespace inside", in: "us er@example.com", wantErr: true},
		{name: "missing local", in: "@example.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.ValidateEmail(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				if !errors.Is(err, auth.ErrInvalidInput) {
					t.Errorf("expected ErrInvalidInput, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
