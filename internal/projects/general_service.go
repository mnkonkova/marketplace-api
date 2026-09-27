package projects

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	generalTitleMin    = 3
	generalTitleMax    = 200
	generalBriefMax    = 5000
	generalNoteMax     = 2000
	generalMaxMaterial = 20
	materialTitleMax   = 200
	materialURLMax     = 2000
	// generalMaxHorizon — дальше года срок не ставится. Это не про
	// технику: проект с дедлайном через три года никто не ведёт, а опечатка
	// в годе («2027» вместо «2026») иначе проходит молча.
	generalMaxHorizon = 365 * 24 * time.Hour
)

// CreateGeneral — проверить и завести общий проект.
func (s *Service) CreateGeneral(ctx context.Context, in CreateGeneralInput) (GeneralProject, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Brief = strings.TrimSpace(in.Brief)

	if n := len([]rune(in.Title)); n < generalTitleMin || n > generalTitleMax {
		return GeneralProject{}, fmt.Errorf("%w: title must be %d-%d chars",
			ErrInvalidInput, generalTitleMin, generalTitleMax)
	}
	if len([]rune(in.Brief)) > generalBriefMax {
		return GeneralProject{}, fmt.Errorf("%w: brief is too long (max %d)", ErrInvalidInput, generalBriefMax)
	}
	if in.SpecialistID == uuid.Nil {
		return GeneralProject{}, fmt.Errorf("%w: specialist_user_id is required", ErrInvalidInput)
	}
	// Заказать у себя — не сценарий, а способ обойти проверки: проект с
	// одним и тем же человеком по обе стороны сдаётся сам себе.
	if in.SpecialistID == in.ClientID {
		return GeneralProject{}, fmt.Errorf("%w: cannot order from yourself", ErrInvalidInput)
	}
	if in.Budget != nil && *in.Budget <= 0 {
		return GeneralProject{}, fmt.Errorf("%w: budget must be positive", ErrInvalidInput)
	}

	// Срок — дата, а не момент. Сравниваем по дате в UTC: «сегодня» ещё
	// допустимо, вчерашнее число — уже нет.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	due := in.DueDate.UTC().Truncate(24 * time.Hour)
	if due.Before(today) {
		return GeneralProject{}, fmt.Errorf("%w: due_date is in the past", ErrInvalidInput)
	}
	if due.After(today.Add(generalMaxHorizon)) {
		return GeneralProject{}, fmt.Errorf("%w: due_date is too far ahead (max 1 year)", ErrInvalidInput)
	}
	in.DueDate = due

	id, err := s.repo.CreateGeneral(ctx, in)
	if err != nil {
		return GeneralProject{}, err
	}
	return s.repo.GetGeneral(ctx, id, in.ClientID)
}

// GetGeneral — карточка для клиента или исполнителя.
func (s *Service) GetGeneral(ctx context.Context, projectID, viewer uuid.UUID) (GeneralProject, error) {
	return s.repo.GetGeneral(ctx, projectID, viewer)
}

// ListGeneral — «мои общие проекты» с той или другой стороны.
func (s *Service) ListGeneral(ctx context.Context, userID uuid.UUID, asClient bool) ([]GeneralProject, error) {
	return s.repo.ListGeneral(ctx, userID, asClient)
}

// Deliver — сдача работы исполнителем.
func (s *Service) Deliver(ctx context.Context, in DeliverInput) (Delivery, error) {
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > generalNoteMax {
		return Delivery{}, fmt.Errorf("%w: note is too long (max %d)", ErrInvalidInput, generalNoteMax)
	}
	if len(in.Materials) > generalMaxMaterial {
		return Delivery{}, fmt.Errorf("%w: too many materials (max %d)", ErrInvalidInput, generalMaxMaterial)
	}
	// Сдача без единого материала и без комментария — пустой клик:
	// клиенту нечего смотреть и не о чем принимать решение.
	if len(in.Materials) == 0 && in.Note == "" {
		return Delivery{}, fmt.Errorf("%w: attach a link or write what was done", ErrInvalidInput)
	}
	for i := range in.Materials {
		m, err := validateMaterial(in.Materials[i])
		if err != nil {
			return Delivery{}, err
		}
		in.Materials[i] = m
	}
	return s.repo.Deliver(ctx, in)
}

func validateMaterial(m MaterialInput) (MaterialInput, error) {
	m.Kind = strings.TrimSpace(m.Kind)
	m.Title = strings.TrimSpace(m.Title)
	m.URL = strings.TrimSpace(m.URL)

	switch m.Kind {
	case "doc", "video", "link":
	default:
		return m, fmt.Errorf("%w: material kind must be doc, video or link", ErrInvalidInput)
	}
	if m.Title == "" || len([]rune(m.Title)) > materialTitleMax {
		return m, fmt.Errorf("%w: material title must be 1-%d chars", ErrInvalidInput, materialTitleMax)
	}
	if len(m.URL) > materialURLMax {
		return m, fmt.Errorf("%w: material url is too long", ErrInvalidInput)
	}
	// Схему проверяем явно: ссылка попадает в интерфейс клиента как
	// кликабельная, и javascript: в ней — исполняемый код у него в браузере.
	u, err := url.Parse(m.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return m, fmt.Errorf("%w: material url must be an http(s) link", ErrInvalidInput)
	}
	return m, nil
}

// AcceptDelivery — клиент принимает работу. Проект закрывается.
func (s *Service) AcceptDelivery(ctx context.Context, projectID, clientID uuid.UUID) (GeneralProject, error) {
	return s.repo.DecideDelivery(ctx, projectID, clientID, true, "")
}

// ReworkDelivery — клиент возвращает работу с причиной. Причина
// обязательна: «доработай» без указания что именно — это ещё один круг
// переписки, а не правка.
func (s *Service) ReworkDelivery(ctx context.Context, projectID, clientID uuid.UUID, reason string) (GeneralProject, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return GeneralProject{}, fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}
	if len([]rune(reason)) > generalNoteMax {
		return GeneralProject{}, fmt.Errorf("%w: reason is too long (max %d)", ErrInvalidInput, generalNoteMax)
	}
	return s.repo.DecideDelivery(ctx, projectID, clientID, false, reason)
}

// CancelGeneral — клиент отменяет проект.
func (s *Service) CancelGeneral(ctx context.Context, projectID, clientID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > generalNoteMax {
		return fmt.Errorf("%w: reason is too long (max %d)", ErrInvalidInput, generalNoteMax)
	}
	return s.repo.CancelGeneral(ctx, projectID, clientID, reason)
}
