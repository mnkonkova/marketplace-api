package publications

import (
	"errors"
	"testing"
)

// Ссылку на аккаунт копируют из адресной строки и из шапки приложения —
// там она сплошь и рядом без схемы. Отказ на этом месте читался как
// «ссылка неправильная», хотя ровно её же ParseLink принимает при сдаче
// ролика: две разные строгости к одному адресу — ловушка, а не защита.
func TestNormalizeAccountURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"tiktok.com/@nastya", "https://tiktok.com/@nastya"},
		{"  www.instagram.com/nastya/  ", "https://www.instagram.com/nastya/"},
		{"https://vk.com/nastya", "https://vk.com/nastya"},
		{"http://dzen.ru/nastya", "http://dzen.ru/nastya"},
	}
	for _, c := range ok {
		got, err := normalizeAccountURL(c.in)
		if err != nil {
			t.Errorf("normalizeAccountURL(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeAccountURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Чужая схема — не формальность: адрес уходит в href, и javascript:
	// там исполняется на странице заказчика.
	bad := []string{"javascript:alert(1)", "data:text/html,<script>", "ftp://files.example/a", "просто текст"}
	for _, in := range bad {
		if _, err := normalizeAccountURL(in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("normalizeAccountURL(%q) прошёл, а не должен", in)
		}
	}
}
