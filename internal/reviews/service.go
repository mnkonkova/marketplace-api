package reviews

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidInput = errors.New("invalid input")

const (
	textMaxLen   = 2000
	// Минимум 3 символа — достаточно для «топ» / «огонь», но отсекает
	// случайные пробелы и однобуквенный спам. lead-привязанный отзыв
	// проходит и без текста (пустая строка тоже валидна).
	textMinNoLead = 3
)

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, in CreateInput) (Review, error) {
	in.Text = strings.TrimSpace(in.Text)

	// Pure-input валидация (без БД) — то что не зависит от состояния.
	// Сообщения на русском — handler передаёт их в errorBody.message,
	// фронт показывает текст напрямую через NzMessageService.
	if in.Rating < 1 || in.Rating > 5 {
		return Review{}, fmt.Errorf("%w: выберите оценку от 1 до 5 звёзд", ErrInvalidInput)
	}
	if in.TargetUserID == uuid.Nil {
		return Review{}, fmt.Errorf("%w: не указан специалист, которому оставляете отзыв", ErrInvalidInput)
	}
	if in.AuthorUserID == in.TargetUserID {
		return Review{}, fmt.Errorf("%w: нельзя оставить отзыв самому себе", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Text) > textMaxLen {
		return Review{}, fmt.Errorf("%w: текст слишком длинный (максимум %d символов)", ErrInvalidInput, textMaxLen)
	}
	if in.LeadID == nil && utf8.RuneCountInString(in.Text) < textMinNoLead {
		return Review{}, fmt.Errorf("%w: напишите хотя бы %d символа в тексте отзыва", ErrInvalidInput, textMinNoLead)
	}

	// Все state-зависимые проверки (TargetIsSpecialist, LeadAuthorizesReview)
	// и резолв author_name теперь делаются ВНУТРИ Repo.Create в одной
	// транзакции — иначе между check и INSERT окно для гонки (data-sec D9),
	// плюс это место единое для проверки прав (data-sec D7 — имя из БД).
	id, err := s.repo.Create(ctx, in)
	if err != nil {
		return Review{}, err
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Update(ctx context.Context, id, authorID uuid.UUID, in UpdateInput) (Review, error) {
	if in.Rating == nil && in.Text == nil {
		return Review{}, fmt.Errorf("%w: нечего обновлять — поменяйте оценку или текст", ErrInvalidInput)
	}
	if in.Rating != nil && (*in.Rating < 1 || *in.Rating > 5) {
		return Review{}, fmt.Errorf("%w: выберите оценку от 1 до 5 звёзд", ErrInvalidInput)
	}
	if in.Text != nil {
		t := strings.TrimSpace(*in.Text)
		if utf8.RuneCountInString(t) > textMaxLen {
			return Review{}, fmt.Errorf("%w: текст слишком длинный (максимум %d символов)", ErrInvalidInput, textMaxLen)
		}
		in.Text = &t
	}
	if _, err := s.repo.Update(ctx, id, authorID, in); err != nil {
		return Review{}, err
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id, authorID uuid.UUID) error {
	_, err := s.repo.Delete(ctx, id, authorID)
	return err
}

func (s *Service) ListByTarget(ctx context.Context, targetID uuid.UUID, limit, offset int) ([]Review, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.ListByTarget(ctx, targetID, limit, offset)
}
