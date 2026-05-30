package productions

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidInput = errors.New("invalid input")

const (
	nameMin = 2
	nameMax = 120
	descMax = 1000
)

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// ListActive — выдача для публичного GET /productions.
func (s *Service) ListActive(ctx context.Context) ([]Production, error) {
	return s.repo.ListActive(ctx)
}

// ListAll — admin: и активные, и архивные.
func (s *Service) ListAll(ctx context.Context) ([]Production, error) {
	return s.repo.ListAll(ctx)
}

func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (Production, error) {
	return s.repo.GetByID(ctx, id)
}

// Create — admin. Валидируем формат + явная pre-check дубля среди активных
// (даёт стабильный ErrDuplicateName поверх БД-индекса; race с параллельным
// INSERT всё равно ловится в repo через isUniqueViolation).
func (s *Service) Create(ctx context.Context, in CreateInput) (Production, error) {
	name, desc, err := normalizeAndValidate(in.Name, in.Description)
	if err != nil {
		return Production{}, err
	}
	n, err := s.repo.CountActiveByLowerName(ctx, name, nil)
	if err != nil {
		return Production{}, err
	}
	if n > 0 {
		return Production{}, ErrDuplicateName
	}
	return s.repo.Create(ctx, CreateInput{Name: name, Description: desc})
}

// Update — admin PATCH. Перед записью валидируем то, что задано, и pre-check
// дубля имени среди активных (исключая саму запись).
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Production, error) {
	out := UpdateInput{IsActive: in.IsActive, UpdatedAt: in.UpdatedAt}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if err := validateName(name); err != nil {
			return Production{}, err
		}
		out.Name = &name
	}
	if in.Description != nil {
		desc := strings.TrimSpace(*in.Description)
		if err := validateDescription(desc); err != nil {
			return Production{}, err
		}
		out.Description = &desc
	}

	if out.Name != nil {
		// Если запись будет деактивирована в этом же PATCH (IsActive=false),
		// дубль среди активных не важен — partial index это позволяет.
		// Но если активность остаётся true, проверим явно.
		willBeActive := true
		if in.IsActive != nil {
			willBeActive = *in.IsActive
		}
		if willBeActive {
			n, err := s.repo.CountActiveByLowerName(ctx, *out.Name, &id)
			if err != nil {
				return Production{}, err
			}
			if n > 0 {
				return Production{}, ErrDuplicateName
			}
		}
	}

	return s.repo.Update(ctx, id, out)
}

// Deactivate — DELETE /admin/productions/{id} = soft-delete (is_active=false).
func (s *Service) Deactivate(ctx context.Context, id uuid.UUID, expectedUpdatedAt *time.Time) (Production, error) {
	return s.repo.Deactivate(ctx, id, expectedUpdatedAt)
}

// ExistsActive — для profiles.Service: валидация production_id в PATCH /me/profile.
func (s *Service) ExistsActive(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.repo.ExistsActive(ctx, id)
}

func normalizeAndValidate(name, desc string) (string, string, error) {
	n := strings.TrimSpace(name)
	d := strings.TrimSpace(desc)
	if err := validateName(n); err != nil {
		return "", "", err
	}
	if err := validateDescription(d); err != nil {
		return "", "", err
	}
	return n, d, nil
}

func validateName(n string) error {
	c := utf8.RuneCountInString(n)
	if c < nameMin || c > nameMax {
		return ErrInvalidInput
	}
	return nil
}

func validateDescription(d string) error {
	if utf8.RuneCountInString(d) > descMax {
		return ErrInvalidInput
	}
	return nil
}
