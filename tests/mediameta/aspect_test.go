package mediameta_test

import (
	"testing"

	"marketpclce/internal/mediameta"
)

func TestAspectFromSize(t *testing.T) {
	cases := []struct {
		name   string
		w, h   int
		expect string
	}{
		{"вертикальный 1080p", 1080, 1920, "9:16"},
		{"горизонтальный 1080p", 1920, 1080, "16:9"},
		{"квадрат", 1080, 1080, "1:1"},
		{"инстаграм 4:5", 1080, 1350, "4:5"},
		{"кинематографичный", 2390, 1000, "239:100"},
		// Кроп на пару пикселей не должен рождать новый «формат».
		{"9:16 после кропа", 1080, 1919, "9:16"},
		{"16:9 после кропа", 1918, 1080, "16:9"},
		// Экзотика вне допуска — сокращаем честно.
		{"нестандартный", 1000, 1400, "5:7"},
		{"нулевая ширина", 0, 1920, ""},
		{"отрицательная высота", 1080, -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mediameta.AspectFromSize(c.w, c.h); got != c.expect {
				t.Errorf("AspectFromSize(%d, %d) = %q, ожидали %q", c.w, c.h, got, c.expect)
			}
		})
	}
}

func TestNormalizeAspect(t *testing.T) {
	cases := []struct {
		in     string
		expect string
	}{
		{"9:16", "9:16"},
		{"1080:1920", "9:16"},
		{" 16 : 9 ", "16:9"},
		{"2.39:1", "239:100"},
		{"", ""},
		{"16/9", ""},
		{"0:16", ""},
		{"-9:16", ""},
		{"abc:def", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := mediameta.NormalizeAspect(c.in); got != c.expect {
				t.Errorf("NormalizeAspect(%q) = %q, ожидали %q", c.in, got, c.expect)
			}
		})
	}
}

// Пустой/битый aspect трактуем как вертикаль — в портфолио это подавляющее
// большинство, и ошибка в эту сторону дешевле (плитка не станет широкой).
func TestIsLandscape(t *testing.T) {
	cases := []struct {
		in     string
		expect bool
	}{
		{"16:9", true},
		{"239:100", true},
		{"9:16", false},
		{"1:1", false},
		{"", false},
		{"мусор", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := mediameta.IsLandscape(c.in); got != c.expect {
				t.Errorf("IsLandscape(%q) = %v, ожидали %v", c.in, got, c.expect)
			}
		})
	}
}
