package publications

import (
	"errors"
	"fmt"
	"time"
)

// Быстрые схемы простановки дат (требование М2: «ставить тридцать штук по
// одной недопустимо»). Менеджер выбирает креаторов, схему и границы месяца —
// выкладки создаются пачкой.
type Scheme string

const (
	SchemeDaily         Scheme = "daily"           // каждый день
	SchemeWeekdays      Scheme = "weekdays"        // будни
	SchemeTueThu        Scheme = "tue_thu"         // вторник и четверг
	SchemeEveryOtherDay Scheme = "every_other_day" // через день
)

// ErrUnknownScheme — схема не из списка. Отдельная ошибка, чтобы фронт мог
// показать список доступных, а не «invalid input».
var ErrUnknownScheme = errors.New("неизвестная схема простановки дат")

// ErrRangeTooLong — защита от простановки на годы вперёд одним нажатием.
// 200 дней с запасом покрывает полгода: за пределами этого почти наверняка
// опечатка в дате, а не намерение.
var ErrRangeTooLong = errors.New("слишком большой диапазон дат")

const maxRangeDays = 200

// GenerateDates — даты по схеме в границах [from, to] включительно.
//
// Чистая функция без обращения к БД: именно её проверяет тест перед тем,
// как результат превратится в шестьдесят строк в project_publications.
func GenerateDates(scheme Scheme, from, to time.Time) ([]time.Time, error) {
	from, to = truncateDay(from), truncateDay(to)
	if to.Before(from) {
		return nil, fmt.Errorf("%w: конец раньше начала", ErrRangeTooLong)
	}
	if int(to.Sub(from).Hours()/24) > maxRangeDays {
		return nil, fmt.Errorf("%w: больше %d дней", ErrRangeTooLong, maxRangeDays)
	}

	var keep func(d time.Time, index int) bool
	switch scheme {
	case SchemeDaily:
		keep = func(time.Time, int) bool { return true }
	case SchemeWeekdays:
		keep = func(d time.Time, _ int) bool {
			wd := d.Weekday()
			return wd != time.Saturday && wd != time.Sunday
		}
	case SchemeTueThu:
		keep = func(d time.Time, _ int) bool {
			wd := d.Weekday()
			return wd == time.Tuesday || wd == time.Thursday
		}
	case SchemeEveryOtherDay:
		// Считаем от начала диапазона, а не от чётности числа: иначе
		// «через день» ломается на переходе через месяц.
		keep = func(_ time.Time, i int) bool { return i%2 == 0 }
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownScheme, scheme)
	}

	out := make([]time.Time, 0, 32)
	for d, i := from, 0; !d.After(to); d, i = d.AddDate(0, 0, 1), i+1 {
		if keep(d, i) {
			out = append(out, d)
		}
	}
	return out, nil
}

// KnownSchemes — то, что показывается менеджеру списком.
func KnownSchemes() []Scheme {
	return []Scheme{SchemeDaily, SchemeWeekdays, SchemeTueThu, SchemeEveryOtherDay}
}
