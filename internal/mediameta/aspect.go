// Package mediameta — общие для доменов утилиты вокруг метаданных медиа.
// Живёт отдельно, потому что нужен и profiles (валидация входного aspect
// при создании работы), и transcode (запись реального формата по ffprobe);
// прямой импорт одного из них другим дал бы цикл.
package mediameta

import (
	"fmt"
	"strconv"
	"strings"
)

// standardAspects — форматы, к которым притягиваем близкие значения. Иначе
// 1080×1919 (кроп на кадр) дал бы "1080:1919" вместо честного "9:16", и на
// фронте плитки в ленте разъезжались бы на пиксель-другой.
var standardAspects = []struct {
	label string
	ratio float64
}{
	{"9:16", 9.0 / 16.0},
	{"3:4", 3.0 / 4.0},
	{"2:3", 2.0 / 3.0},
	{"4:5", 4.0 / 5.0},
	{"1:1", 1.0},
	{"5:4", 5.0 / 4.0},
	{"4:3", 4.0 / 3.0},
	{"3:2", 3.0 / 2.0},
	{"16:9", 16.0 / 9.0},
	{"239:100", 2.39},
}

// aspectTolerance — относительный допуск при притягивании к стандарту.
// 2.5% отделяет «тот же формат после кропа» от соседнего стандарта:
// ближайшие пары (4:5 vs 1:1, 3:2 vs 16:9) расходятся на 20%+.
const aspectTolerance = 0.025

// AspectFromSize возвращает канонический «W:H» по размерам кадра.
// Пустая строка — если размеры невалидны (нулевые/отрицательные).
//
// Значение сначала притягивается к стандартному формату, а если ни один не
// подошёл — сокращается через НОД. Совсем экзотические соотношения (после
// сокращения остаются трёхзначные числа) округляются до «N:100», чтобы в
// БД не оседали строки вида "1001:1920".
func AspectFromSize(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	ratio := float64(width) / float64(height)
	for _, std := range standardAspects {
		if relativeDiff(ratio, std.ratio) <= aspectTolerance {
			return std.label
		}
	}
	w, h := width/gcd(width, height), height/gcd(width, height)
	if w > 999 || h > 999 {
		return fmt.Sprintf("%d:100", int(ratio*100+0.5))
	}
	return fmt.Sprintf("%d:%d", w, h)
}

// NormalizeAspect приводит присланный клиентом «W:H» к канонической форме.
// Возвращает "" на любом мусоре — вызывающий код в этом случае пишет NULL,
// чтобы не выдавать выдумку за измеренный формат.
func NormalizeAspect(s string) string {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return ""
	}
	w, errW := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, errH := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return ""
	}
	// Дробные значения ("2.39:1") приводим к целым, домножив на 100 — дальше
	// работает обычное притягивание к стандартам.
	return AspectFromSize(int(w*100+0.5), int(h*100+0.5))
}

// IsLandscape — true если формат горизонтальный (шире, чем выше). Пустой
// или невалидный aspect считаем вертикальным: в портфолио вертикаль —
// подавляющее большинство, и ошибка в эту сторону дешевле.
func IsLandscape(aspect string) bool {
	parts := strings.Split(aspect, ":")
	if len(parts) != 2 {
		return false
	}
	w, errW := strconv.ParseFloat(parts[0], 64)
	h, errH := strconv.ParseFloat(parts[1], 64)
	if errW != nil || errH != nil || h <= 0 {
		return false
	}
	return w > h
}

func relativeDiff(a, b float64) float64 {
	if b == 0 {
		return 1
	}
	d := (a - b) / b
	if d < 0 {
		return -d
	}
	return d
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}
