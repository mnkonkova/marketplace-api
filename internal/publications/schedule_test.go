package publications

import (
	"errors"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestGenerateDates(t *testing.T) {
	// Сентябрь 2026: 1-е — вторник.
	from, to := day("2026-09-01"), day("2026-09-14")

	cases := []struct {
		scheme Scheme
		want   []string
	}{
		{SchemeTueThu, []string{
			"2026-09-01", "2026-09-03", "2026-09-08", "2026-09-10",
		}},
		{SchemeWeekdays, []string{
			"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04",
			"2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11",
			// 14-е — понедельник и последний день диапазона: входит.
			"2026-09-14",
		}},
		{SchemeEveryOtherDay, []string{
			"2026-09-01", "2026-09-03", "2026-09-05", "2026-09-07",
			"2026-09-09", "2026-09-11", "2026-09-13",
		}},
	}
	for _, c := range cases {
		t.Run(string(c.scheme), func(t *testing.T) {
			got, err := GenerateDates(c.scheme, from, to)
			if err != nil {
				t.Fatalf("GenerateDates: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("дат %d, ожидалось %d: %v", len(got), len(c.want), format(got))
			}
			for i := range got {
				if got[i].Format("2006-01-02") != c.want[i] {
					t.Errorf("дата %d: got %s, want %s", i,
						got[i].Format("2006-01-02"), c.want[i])
				}
			}
		})
	}
}

func TestGenerateDatesDailyIncludesBothEnds(t *testing.T) {
	got, err := GenerateDates(SchemeDaily, day("2026-09-01"), day("2026-09-03"))
	if err != nil {
		t.Fatalf("GenerateDates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("границы диапазона должны входить: got %v", format(got))
	}
}

// «Через день» отсчитывается от начала диапазона, а не от чётности числа:
// иначе схема ломается на переходе через месяц (31-е и 1-е подряд).
func TestEveryOtherDayCrossesMonthBoundary(t *testing.T) {
	got, err := GenerateDates(SchemeEveryOtherDay, day("2026-08-30"), day("2026-09-03"))
	if err != nil {
		t.Fatalf("GenerateDates: %v", err)
	}
	want := []string{"2026-08-30", "2026-09-01", "2026-09-03"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", format(got), want)
	}
	for i := range want {
		if got[i].Format("2006-01-02") != want[i] {
			t.Errorf("дата %d: got %s, want %s", i, got[i].Format("2006-01-02"), want[i])
		}
	}
}

func TestGenerateDatesRejects(t *testing.T) {
	if _, err := GenerateDates("по настроению", day("2026-09-01"), day("2026-09-02")); !errors.Is(err, ErrUnknownScheme) {
		t.Errorf("неизвестная схема: got %v, want ErrUnknownScheme", err)
	}
	if _, err := GenerateDates(SchemeDaily, day("2026-09-10"), day("2026-09-01")); !errors.Is(err, ErrRangeTooLong) {
		t.Errorf("конец раньше начала: got %v", err)
	}
	if _, err := GenerateDates(SchemeDaily, day("2026-01-01"), day("2027-01-01")); !errors.Is(err, ErrRangeTooLong) {
		t.Errorf("год одним нажатием: got %v", err)
	}
}

func format(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Format("2006-01-02")
	}
	return out
}

// Напоминания уходят утром, а не в 00:05: первый проход после полуночи
// не должен будить креатора.
func TestReminderWindowOpen(t *testing.T) {
	at := func(hour int) time.Time {
		return time.Date(2026, 9, 5, hour, 30, 0, 0, time.UTC)
	}
	cases := []struct {
		hour int
		want bool
	}{
		{0, false}, {5, false}, {8, false},
		// Окно — три часа с девяти. Раньше проверка была открыта сверху,
		// и полный проход запускался пятнадцать раз в сутки: четырнадцать
		// из них ничего не отправляли, но вычитывали все открытые выкладки.
		{9, true}, {10, true}, {11, true},
		{12, false}, {14, false}, {23, false},
	}
	for _, c := range cases {
		if got := ReminderWindowOpen(at(c.hour), DefaultReminderHour); got != c.want {
			t.Errorf("в %02d:30 окно open=%v, ожидалось %v", c.hour, got, c.want)
		}
	}
}
