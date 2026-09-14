// Package ratings — версионируемый справочник порогов оценок.
//
// По каким числам ролик называется плохим, средним, хорошим, отличным,
// хитом или виральным; что считается типичным роликом; какие доли
// уровней нормальны; в каких рамках считать относительную оценку; почём
// тысяча просмотров у других.
//
// Версионируется как прайс, чеклисты и воронки — четвёртый раз тем же
// приёмом. Версия не правится: выпускается следующая. Проект снимает
// копию действующей, подытоженный период помнит свою — оценка это
// утверждение о прошлом, и менять пороги задним числом значит переписать
// то, что клиент уже видел.
//
// Денег здесь нет: гарантия, потолок, ступени и ставки живут в тарифе
// (internal/billing). Два места про одни и те же деньги разъедутся.
package ratings

import (
	"time"

	"github.com/google/uuid"
)

// Уровни ролика по сумме просмотров на пяти площадках.
const (
	LevelBad    = "bad"
	LevelMedium = "medium"
	LevelGood   = "good"
	LevelGreat  = "great"
	LevelHit    = "hit"
	LevelViral  = "viral"
)

// Levels — границы уровней: ниже MediumFrom ролик плохой, от ViralFrom
// виральный. Пять границ на шесть уровней.
type Levels struct {
	MediumFrom int64 `json:"medium_from"`
	GoodFrom   int64 `json:"good_from"`
	GreatFrom  int64 `json:"great_from"`
	HitFrom    int64 `json:"hit_from"`
	ViralFrom  int64 `json:"viral_from"`
}

// Level — как называется ролик с такими просмотрами.
//
// Метод, а не разбросанные по коду сравнения: границ пять, и каждая
// копия этой лесенки однажды разойдётся с остальными.
func (l Levels) Level(views int64) string {
	switch {
	case views >= l.ViralFrom:
		return LevelViral
	case views >= l.HitFrom:
		return LevelHit
	case views >= l.GreatFrom:
		return LevelGreat
	case views >= l.GoodFrom:
		return LevelGood
	case views >= l.MediumFrom:
		return LevelMedium
	default:
		return LevelBad
	}
}

// PlatformLevels — границы одной площадки. Три на четыре уровня: на
// площадке важно «тянет или нет», а не шесть оттенков.
type PlatformLevels struct {
	Platform   string `json:"platform"`
	MediumFrom int64  `json:"medium_from"`
	GoodFrom   int64  `json:"good_from"`
	GreatFrom  int64  `json:"great_from"`
}

// Shares — норма распределения роликов по уровням, в процентах.
// Сумма ровно сто: это доли одного и того же набора.
type Shares struct {
	BadPct    int `json:"bad_pct"`
	MediumPct int `json:"medium_pct"`
	GoodPct   int `json:"good_pct"`
	// GreatPct — отличные и выше: хиты и виральные тоже сюда. Отдельной
	// нормы у них нет — они слишком редки, чтобы ждать их долю.
	GreatPct int `json:"great_pct"`
}

// Relative — рамки относительной оценки. Сами перцентили считаются по
// данным проекта и не настраиваются: настраивается только то, на каком
// окне и по какому минимуму их вообще можно считать.
type Relative struct {
	// WindowDays — за какой период берём ролики для сравнения.
	WindowDays int `json:"window_days"`
	// MinMatureVideos — меньше какого числа зрелых роликов сравнивать
	// бессмысленно: перцентиль по десяти роликам — это не перцентиль.
	MinMatureVideos int `json:"min_mature_videos"`
	// MatureAgeDays — со скольких дней ролик считается зрелым.
	MatureAgeDays int `json:"mature_age_days"`
}

// MarketPrice — ориентир рынка: почём тысяча показов у других.
//
// Источник и дата лежат рядом с числом намеренно: цифра показывается
// клиенту и протухает. Без даты через год она начнёт врать, и заметить
// это будет нечем.
type MarketPrice struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// PricePer1000 — копейки.
	PricePer1000 int64  `json:"price_per_1000"`
	Source       string `json:"source"`
	// MeasuredOn — когда измеряли. Именно дата измерения, а не выпуска
	// версии: числа переносят из версии в версию, и дата должна
	// переезжать вместе с числом.
	MeasuredOn time.Time `json:"measured_on"`
}

// Scale — версия справочника целиком.
type Scale struct {
	ID          uuid.UUID  `json:"id"`
	Version     int        `json:"version"`
	PublishedAt time.Time  `json:"published_at"`
	PublishedBy *uuid.UUID `json:"published_by,omitempty"`
	Note        string     `json:"note,omitempty"`
	// IsCurrent — эта версия сейчас действует. Действует всегда одна:
	// самая новая.
	IsCurrent bool `json:"is_current"`
	// UsedByProjects/UsedByPeriods — сколько проектов сняли с неё копию
	// и сколько подытоженных периодов ею оценены. Показывает, что версию
	// нельзя считать черновиком.
	UsedByProjects int `json:"used_by_projects"`
	UsedByPeriods  int `json:"used_by_periods"`

	Levels            Levels           `json:"levels"`
	PlatformLevels    []PlatformLevels `json:"platform_levels"`
	TypicalVideoViews int64            `json:"typical_video_views"`
	Shares            Shares           `json:"shares"`
	Relative          Relative         `json:"relative"`
	Market            []MarketPrice    `json:"market"`
}

// ScaleChange — одно изменившееся число между версиями.
type ScaleChange struct {
	Field string `json:"field"`
	Label string `json:"label"`
	From  *int64 `json:"from"`
	To    *int64 `json:"to"`
}

// PublishResult — выпущенная версия плюс разница с прежней.
//
// Выпуск необратим: прежнюю версию мы не переписываем, а проекты и
// периоды продолжают жить на ней. Поэтому ответ показывает не только
// новую версию, но и что именно изменилось — как у прайса.
type PublishResult struct {
	Scale Scale `json:"scale"`
	// Changes — разница с прежней действующей версией. Пусто у самой
	// первой: сравнивать не с чем.
	Changes []ScaleChange `json:"changes"`
	// ProjectsOnPrevious — сколько проектов остались на прежней версии.
	// Они за новой не последуют: копия снимается один раз.
	ProjectsOnPrevious int `json:"projects_on_previous"`
}
