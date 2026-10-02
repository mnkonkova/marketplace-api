package projects

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// «Слишком длинно» — отдельная причина (бот объясняет её иначе), но
// по-прежнему ErrInvalidInput с прежним текстом: кабинетные ручки
// отвечают как раньше. До базы такой комментарий не доходит.
func TestCreateCommentTooLongIsTyped(t *testing.T) {
	s := &Service{}
	_, err := s.CreateComment(context.Background(), CommentRequest{
		ProjectID: uuid.New(), AuthorID: uuid.New(),
		Thread: ThreadClient, Body: strings.Repeat("я", commentMaxLen+1), Format: FormatPlain,
	})
	if !errors.Is(err, ErrCommentTooLong) {
		t.Fatalf("ожидали ErrCommentTooLong, получили %v", err)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ErrCommentTooLong перестал быть ErrInvalidInput: %v", err)
	}
	if got, want := err.Error(), "invalid input: body too long (5001 > 5000)"; got != want {
		t.Errorf("текст ошибки %q, был %q", got, want)
	}

	// Прочий некорректный ввод — не «слишком длинно».
	_, err = s.CreateComment(context.Background(), CommentRequest{
		ProjectID: uuid.New(), AuthorID: uuid.New(), Thread: "чужая", Body: "x",
	})
	if !errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrCommentTooLong) {
		t.Errorf("неизвестная ветка: %v", err)
	}
}
