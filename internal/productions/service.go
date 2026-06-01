package productions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidInput = errors.New("invalid input")

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, in CreateInput) (Production, error) {
	if err := validateName(in.Name); err != nil {
		return Production{}, err
	}
	return s.repo.Create(ctx, in)
}

// List — admin видит все; публичный путь использует ListActive.
func (s *Service) List(ctx context.Context) ([]Production, error) {
	return s.repo.List(ctx, false)
}

func (s *Service) ListActive(ctx context.Context) ([]Production, error) {
	return s.repo.List(ctx, true)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Production, error) {
	return s.repo.Get(ctx, id)
}

// IsActiveProduction — реализация profiles.ProductionLookup. Возвращает
// (false, nil) если объекта нет (а не ошибку) — это валидное состояние
// «id невалиден», и сервису profiles удобнее одной булкой ветвиться.
func (s *Service) IsActiveProduction(ctx context.Context, id uuid.UUID) (bool, error) {
	p, err := s.repo.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.IsActive, nil
}

func (s *Service) Patch(ctx context.Context, id uuid.UUID, in PatchInput) (Production, error) {
	if in.Name != nil {
		if err := validateName(*in.Name); err != nil {
			return Production{}, err
		}
	}
	return s.repo.Patch(ctx, id, in)
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}

// ValidateName — экспортируется ради юнит-тестов. Используется и сервисом.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	case utf8.RuneCountInString(name) > 120:
		return fmt.Errorf("%w: name must be <= 120 chars", ErrInvalidInput)
	}
	return nil
}

func validateName(name string) error { return ValidateName(name) }
