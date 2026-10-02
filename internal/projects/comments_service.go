package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"marketpclce/internal/richtext"
)

// commentMaxLen — предел на ТЕКСТ сообщения, без разметки. Считать по
// разметке нельзя: пять абзацев со ссылками занимают втрое больше символов,
// чем видит автор, и лимит срабатывал бы на глаз произвольно.
const commentMaxLen = 5000

// ErrCommentTooLong — текст длиннее commentMaxLen (или разметка больше
// предела richtext). Отдельная причина нужна боту: «слишком длинно» он
// объясняет человеку иначе, чем прочий некорректный ввод.
//
// Это по-прежнему ErrInvalidInput (errors.Is сработает на оба), и текст
// ошибки тот же, что был, — кабинетные ручки отвечают как раньше.
var ErrCommentTooLong error = commentTooLongError{}

type commentTooLongError struct{}

func (commentTooLongError) Error() string { return ErrInvalidInput.Error() + ": body too long" }

func (commentTooLongError) Is(target error) bool { return target == ErrInvalidInput }

// ListThread — одна ветка переписки. Право читать проверяется в handler:
// клиент — через GetClientProject, менеджер — через AssertManagerHasAccess,
// креатор — через ResolveCreatorThread.
func (s *Service) ListThread(ctx context.Context, projectID uuid.UUID, thread string, threadUserID *uuid.UUID) ([]Comment, error) {
	return s.repo.ListThread(ctx, projectID, thread, threadUserID)
}

// ListAllComments — вся переписка проекта: менеджер и админ.
func (s *Service) ListAllComments(ctx context.Context, projectID uuid.UUID) ([]Comment, error) {
	return s.repo.ListAllComments(ctx, projectID)
}

// ThreadParticipants — кандидаты в упоминания для ветки.
func (s *Service) ThreadParticipants(ctx context.Context, projectID uuid.UUID, thread string, threadUserID *uuid.UUID) ([]Participant, error) {
	return s.repo.ThreadParticipants(ctx, projectID, thread, threadUserID)
}

// ResolveCreatorThread — ветка исполнителя в этом проекте (см. репозиторий).
func (s *Service) ResolveCreatorThread(ctx context.Context, projectID, userID uuid.UUID) (string, *uuid.UUID, error) {
	return s.repo.ResolveCreatorThread(ctx, projectID, userID)
}

// CommentRequest — то, что пришло от автора.
type CommentRequest struct {
	ProjectID    uuid.UUID
	AuthorID     uuid.UUID
	Thread       string
	ThreadUserID *uuid.UUID
	Body         string
	// Format — plain или html. Пусто = plain.
	Format string
}

// CreateComment — чистка разметки, проверка упоминаний и запись.
//
// Разметку чистим здесь, а не на фронте и не при чтении. На фронте — потому
// что запрос можно послать мимо него; при чтении — потому что тогда цена
// чистки умножается на число читателей, а вернуть в базу уже безопасное
// значение дешевле и надёжнее.
func (s *Service) CreateComment(ctx context.Context, in CommentRequest) (Comment, error) {
	switch in.Thread {
	case "":
		in.Thread = ThreadClient
	case ThreadClient, ThreadCreator, ThreadInternal:
	default:
		return Comment{}, fmt.Errorf("%w: unknown thread %q", ErrInvalidInput, in.Thread)
	}
	if in.Thread == ThreadCreator && in.ThreadUserID == nil {
		return Comment{}, fmt.Errorf("%w: creator thread requires a user", ErrInvalidInput)
	}

	body := strings.TrimSpace(in.Body)
	if body == "" {
		return Comment{}, ErrCommentEmpty
	}

	out := CreateCommentInput{
		ProjectID:    in.ProjectID,
		AuthorID:     in.AuthorID,
		Thread:       in.Thread,
		ThreadUserID: in.ThreadUserID,
	}

	switch in.Format {
	case "", FormatPlain:
		out.BodyFormat = FormatPlain
		out.Body = body
		out.BodyText = richtext.PlainText(body)
	case FormatHTML:
		// Упомянуть можно только участника ветки. Список тянем один раз и
		// закрываем им предикат — иначе на каждое упоминание уходил бы
		// отдельный запрос.
		people, err := s.repo.ThreadParticipants(ctx, in.ProjectID, in.Thread, in.ThreadUserID)
		if err != nil {
			return Comment{}, err
		}
		allowed := make(map[uuid.UUID]bool, len(people))
		for _, p := range people {
			allowed[p.UserID] = true
		}
		res, err := richtext.Sanitize(body, func(id uuid.UUID) bool { return allowed[id] })
		switch {
		case errors.Is(err, richtext.ErrEmpty):
			return Comment{}, ErrCommentEmpty
		case errors.Is(err, richtext.ErrTooLarge):
			return Comment{}, ErrCommentTooLong
		case err != nil:
			return Comment{}, fmt.Errorf("%w: cannot parse markup", ErrInvalidInput)
		}
		out.BodyFormat = FormatHTML
		out.Body = res.HTML
		out.BodyText = res.Text
		out.Mentions = res.Mentions
	default:
		return Comment{}, fmt.Errorf("%w: unknown body_format %q", ErrInvalidInput, in.Format)
	}

	// Длину меряем по тексту: см. commentMaxLen. Считаем в рунах —
	// в байтах кириллица вдвое длиннее, и лимит для русского текста
	// оказался бы вдвое меньше заявленного.
	if n := len([]rune(out.BodyText)); n > commentMaxLen {
		return Comment{}, fmt.Errorf("%w (%d > %d)", ErrCommentTooLong, n, commentMaxLen)
	}

	return s.repo.CreateComment(ctx, out)
}

// ListEvents — лента активности. Авторизация — в handler.
func (s *Service) ListEvents(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]Event, error) {
	events, err := s.repo.ListEvents(ctx, projectID, limit, offset)
	if err != nil {
		return nil, err
	}
	return events, nil
}
