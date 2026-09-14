package ratings

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
)

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// List — все версии справочника, свежие первыми.
func (s *Service) List(ctx context.Context) ([]Scale, error) { return s.repo.List(ctx) }

// Current — действующая версия.
func (s *Service) Current(ctx context.Context) (Scale, error) { return s.repo.Current(ctx) }

// ForProject — версия, по которой оценивается проект. Снимает копию,
// если её ещё нет.
func (s *Service) ForProject(ctx context.Context, projectID uuid.UUID) (Scale, error) {
	return s.repo.ForProject(ctx, projectID)
}

// Publish — проверить и выпустить новую версию.
//
// Проверки здесь, а не только в базе: CHECK скажет «нарушено
// ограничение», а человеку нужно знать, какое именно число он ввёл не
// так. База при этом остаётся вторым рубежом — она ловит то, что пришло
// мимо этого кода.
func (s *Service) Publish(ctx context.Context, in Scale, actor uuid.UUID) (PublishResult, error) {
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > 500 {
		return PublishResult{}, fmt.Errorf("%w: пояснение слишком длинное", ErrInvalidInput)
	}

	lv := in.Levels
	switch {
	case lv.MediumFrom <= 0:
		return PublishResult{}, fmt.Errorf("%w: граница среднего ролика должна быть больше нуля", ErrInvalidInput)
	case lv.MediumFrom >= lv.GoodFrom, lv.GoodFrom >= lv.GreatFrom,
		lv.GreatFrom >= lv.HitFrom, lv.HitFrom >= lv.ViralFrom:
		return PublishResult{}, fmt.Errorf(
			"%w: границы уровней должны расти: средний < хороший < отличный < хит < виральный", ErrInvalidInput)
	}
	if in.TypicalVideoViews <= 0 {
		return PublishResult{}, fmt.Errorf("%w: типичный ролик должен быть больше нуля", ErrInvalidInput)
	}

	sh := in.Shares
	if sum := sh.BadPct + sh.MediumPct + sh.GoodPct + sh.GreatPct; sum != 100 {
		return PublishResult{}, fmt.Errorf(
			"%w: доли уровней должны складываться в 100%%, а не в %d%%", ErrInvalidInput, sum)
	}
	if sh.BadPct < 0 || sh.MediumPct < 0 || sh.GoodPct < 0 || sh.GreatPct < 0 {
		return PublishResult{}, fmt.Errorf("%w: доли не бывают отрицательными", ErrInvalidInput)
	}

	rel := in.Relative
	if rel.WindowDays <= 0 || rel.MinMatureVideos <= 0 || rel.MatureAgeDays <= 0 {
		return PublishResult{}, fmt.Errorf(
			"%w: окно, минимум роликов и зрелость должны быть больше нуля", ErrInvalidInput)
	}

	// Площадки: все пять и без чужих. Неполный набор означал бы, что у
	// части площадок порогов нет вовсе, и ролик там нельзя ни назвать
	// хорошим, ни плохим.
	seen := map[string]bool{}
	for _, p := range in.PlatformLevels {
		if !publications.IsKnownPlatform(p.Platform) {
			return PublishResult{}, fmt.Errorf(
				"%w: неизвестная площадка %q", ErrInvalidInput, p.Platform)
		}
		if seen[p.Platform] {
			return PublishResult{}, fmt.Errorf("%w: площадка %q задана дважды", ErrInvalidInput, p.Platform)
		}
		seen[p.Platform] = true
		if p.MediumFrom <= 0 || p.MediumFrom >= p.GoodFrom || p.GoodFrom >= p.GreatFrom {
			return PublishResult{}, fmt.Errorf(
				"%w: у площадки %s границы должны расти и быть больше нуля", ErrInvalidInput, p.Platform)
		}
	}
	for _, p := range publications.AllPlatforms {
		if !seen[p] {
			return PublishResult{}, fmt.Errorf(
				"%w: не задана площадка %s — без порогов её ролики не оценить", ErrInvalidInput, p)
		}
	}

	// Рынок: число без источника и даты через год начнёт врать, а
	// заметить это будет нечем.
	keys := map[string]bool{}
	for i := range in.Market {
		m := &in.Market[i]
		m.Key = strings.TrimSpace(m.Key)
		m.Title = strings.TrimSpace(m.Title)
		m.Source = strings.TrimSpace(m.Source)
		if m.Key == "" || m.Title == "" {
			return PublishResult{}, fmt.Errorf("%w: у ориентира рынка нужны ключ и название", ErrInvalidInput)
		}
		if keys[m.Key] {
			return PublishResult{}, fmt.Errorf("%w: ориентир %q задан дважды", ErrInvalidInput, m.Key)
		}
		keys[m.Key] = true
		if m.Source == "" {
			return PublishResult{}, fmt.Errorf(
				"%w: у ориентира %q нет источника — без него число нечем проверить", ErrInvalidInput, m.Key)
		}
		if m.MeasuredOn.IsZero() {
			return PublishResult{}, fmt.Errorf(
				"%w: у ориентира %q нет даты измерения — без неё он молча протухнет", ErrInvalidInput, m.Key)
		}
		if m.PricePer1000 < 0 {
			return PublishResult{}, fmt.Errorf("%w: цена ориентира %q отрицательная", ErrInvalidInput, m.Key)
		}
	}

	return s.repo.Publish(ctx, in, actor)
}
