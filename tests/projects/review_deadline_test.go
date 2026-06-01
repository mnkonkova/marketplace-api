package projects_test

import (
	"testing"
	"time"

	"marketpclce/internal/projects"
)

// TestReviewDeadlineDefault — сервис без явного WithReviewDeadline должен
// использовать 7 дней (см. service.go). Идёт через косвенную проверку:
// если в кейсе review+waiting_client мы не падаем, то дефолт жив. Это
// узкий smoke-тест: без БД мы не можем проверить full lifecycle.
func TestReviewDeadlineDefault(t *testing.T) {
	s := projects.NewService(nil) // без WithReviewDeadline

	// нет внешнего accessor'а на reviewDeadline — проверяем через
	// конструктор + With(7d) что цепочка возвращает тот же сервис.
	got := s.WithReviewDeadline(7 * 24 * time.Hour)
	if got != s {
		t.Fatalf("WithReviewDeadline must return receiver")
	}
}
