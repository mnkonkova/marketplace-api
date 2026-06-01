package projects_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
)

// Этот файл — лёгкие smoke-тесты для логики менеджерского сервиса, которые
// не требуют БД. На полную интеграцию (advance_stage с реальными данными)
// нужен test DB harness — этого нет в текущем стеке, отложено.

// TestSkipRequiresComment проверяет, что SkipStep отклоняет пустой коммент.
// Используем сервис без репо: первая проверка идёт до обращения к БД.
func TestSkipRequiresComment(t *testing.T) {
	// сервис с nil-репо — не дойдёт до БД на первой ветке
	s := projects.NewService(nil)

	_, err := s.SkipStep(context.Background(), uuid.New(), uuid.New(), uuid.New(), "   ")
	if err == nil {
		t.Fatalf("want error on empty comment")
	}
	if !errors.Is(err, projects.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

// AssertManagerHasAccess после фикса аудита принимает uuid.Nil как маркер
// «вызов от админа» — обращается в репо без assigned_to-фильтра. Логика
// проверки нужна на integration-уровне (с реальной БД); юнит-тесту
// проверять нечего.
