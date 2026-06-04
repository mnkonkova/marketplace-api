package reviews_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/reviews"
)

// Тесты валидаций в reviews.Service.Create/Update/ListByTarget.
// Используем nil-repo — все валидации в Create отрабатывают ДО первого
// обращения к репо, так что для negative-кейсов БД не нужна.

func TestCreate_RejectsBadRating(t *testing.T) {
	s := reviews.NewService(nil)
	cases := []int{0, -1, 6, 100}
	for _, r := range cases {
		_, err := s.Create(context.Background(), reviews.CreateInput{
			Rating:       r,
			TargetUserID: uuid.New(),
			AuthorUserID: uuid.New(),
			Text:         "Good work",
		})
		if !errors.Is(err, reviews.ErrInvalidInput) {
			t.Errorf("rating=%d: want ErrInvalidInput, got %v", r, err)
		}
	}
}

func TestCreate_RejectsEmptyTarget(t *testing.T) {
	s := reviews.NewService(nil)
	_, err := s.Create(context.Background(), reviews.CreateInput{
		Rating:       5,
		TargetUserID: uuid.Nil,
		AuthorUserID: uuid.New(),
		Text:         "Hi there",
	})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for empty target, got %v", err)
	}
}

func TestCreate_RejectsSelfReview(t *testing.T) {
	s := reviews.NewService(nil)
	uid := uuid.New()
	_, err := s.Create(context.Background(), reviews.CreateInput{
		Rating:       5,
		TargetUserID: uid,
		AuthorUserID: uid,
		Text:         "self praise",
	})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for self-review, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "самому") {
		t.Errorf("error message should mention self-review: %v", err)
	}
}

func TestCreate_RejectsTooLongText(t *testing.T) {
	s := reviews.NewService(nil)
	longText := strings.Repeat("я", 2001) // > textMaxLen=2000 рун
	_, err := s.Create(context.Background(), reviews.CreateInput{
		Rating:       5,
		TargetUserID: uuid.New(),
		AuthorUserID: uuid.New(),
		Text:         longText,
	})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for long text, got %v", err)
	}
}

func TestCreate_RejectsTooLongAuthorName(t *testing.T) {
	s := reviews.NewService(nil)
	_, err := s.Create(context.Background(), reviews.CreateInput{
		Rating:       5,
		TargetUserID: uuid.New(),
		AuthorUserID: uuid.New(),
		AuthorName:   strings.Repeat("X", 200),
		Text:         "Good",
	})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for long author name, got %v", err)
	}
}

func TestCreate_RejectsShortTextWithoutLead(t *testing.T) {
	s := reviews.NewService(nil)
	_, err := s.Create(context.Background(), reviews.CreateInput{
		Rating:       5,
		TargetUserID: uuid.New(),
		AuthorUserID: uuid.New(),
		LeadID:       nil,    // нет лида — текст обязателен
		Text:         "ок",   // 2 руны, минимум 3
	})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for short text without lead, got %v", err)
	}
}

// ---- Update ----

func TestUpdate_RejectsNothingToUpdate(t *testing.T) {
	s := reviews.NewService(nil)
	_, err := s.Update(context.Background(), uuid.New(), uuid.New(), reviews.UpdateInput{})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput when no fields to update, got %v", err)
	}
}

func TestUpdate_RejectsBadRating(t *testing.T) {
	s := reviews.NewService(nil)
	bad := 7
	_, err := s.Update(context.Background(), uuid.New(), uuid.New(), reviews.UpdateInput{Rating: &bad})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for rating=7, got %v", err)
	}
}

func TestUpdate_RejectsTooLongText(t *testing.T) {
	s := reviews.NewService(nil)
	t1 := strings.Repeat("X", 2001)
	_, err := s.Update(context.Background(), uuid.New(), uuid.New(), reviews.UpdateInput{Text: &t1})
	if !errors.Is(err, reviews.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput for >2000 chars, got %v", err)
	}
}
