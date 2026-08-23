package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

// Лимит должен считаться по человеку, когда человек известен.
//
// Иначе получается двойная несправедливость: офис за одним NAT'ом делит
// корзину на всех сотрудников, а тот, кто перебирает, лимит обходит — сменить
// адрес дешевле, чем завести аккаунт.
func TestKeyIsUserWhenAuthenticated(t *testing.T) {
	id := uuid.New()
	r := httptest.NewRequest("POST", "/api/v1/me/uploads/image", nil)
	r.RemoteAddr = "10.0.0.7:5555"
	r = r.WithContext(auth.WithUserID(r.Context(), id))

	if got, want := clientKey(r), "u:"+id.String(); got != want {
		t.Fatalf("ключ %q, ожидали %q", got, want)
	}
}

// А где человека ещё нет — логин, регистрация, каталог — считаем по адресу:
// это и есть анти-брутфорс, и другого ключа там просто не существует.
func TestKeyFallsBackToAddress(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	r.RemoteAddr = "10.0.0.7:5555"

	if got, want := clientKey(r), "10.0.0.7"; got != want {
		t.Fatalf("ключ %q, ожидали %q", got, want)
	}
}

// Два аккаунта с одного адреса не должны делить корзину: именно на этом
// ломался лимит для офиса и общего wi-fi.
func TestTwoUsersOneAddressAreCountedApart(t *testing.T) {
	first := httptest.NewRequest("POST", "/api/v1/me/uploads/image", nil)
	first.RemoteAddr = "10.0.0.7:5555"
	first = first.WithContext(auth.WithUserID(first.Context(), uuid.New()))

	second := httptest.NewRequest("POST", "/api/v1/me/uploads/image", nil)
	second.RemoteAddr = "10.0.0.7:6666"
	second = second.WithContext(auth.WithUserID(second.Context(), uuid.New()))

	if clientKey(first) == clientKey(second) {
		t.Fatal("разные аккаунты с одного адреса получили один ключ")
	}
}
