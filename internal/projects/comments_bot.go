package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"marketpclce/internal/telegram"
)

// Комментарии из телеграм-бота.
//
// Человек ответил боту на пинг — и ответ должен лечь в переписку
// проекта ровно так, как если бы он написал его в кабинете: та же
// ветка, та же запись, то же событие project.comment_added и,
// значит, то же уведомление менеджеру. Поэтому здесь нет своей
// вставки — только выбор ветки и вызов CreateComment.
//
// Реализует telegram.ProjectComments. Интерфейс объявлен там, а не
// здесь, потому что telegram не может импортировать projects: стрелка
// projects → auth → telegram уже есть, и обратная замкнула бы кольцо.

// BotComments — адаптер для сервиса бота.
func (s *Service) BotComments() telegram.ProjectComments { return botComments{s: s} }

type botComments struct{ s *Service }

// memberThread — в какую ветку этот человек пишет из этого бота.
//
// Бот определяет сторону разговора, и выбирать её по роли человека
// нельзя: заказчик, который случайно оказался креатором в чужом
// проекте, из клиентского бота пишет только как заказчик. Менеджер и
// админ здесь не проходят вовсе — в кабинете у них есть все ветки, а
// из бота «какая из трёх» не угадать.
func (b botComments) memberThread(
	ctx context.Context, bot string, projectID, userID uuid.UUID,
) (Project, string, *uuid.UUID, error) {
	p, err := b.s.repo.GetByID(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return Project{}, "", nil, telegram.ErrNoProject
	}
	if err != nil {
		return Project{}, "", nil, err
	}
	switch bot {
	case telegram.BotClient:
		// Заказчик — ровно владелец проекта. Через GetClientProject
		// сюда прошли бы и менеджер с админом («посмотреть глазами
		// клиента»), а писать от их имени в клиентскую ветку из бота
		// незачем.
		if p.ClientUserID == nil || *p.ClientUserID != userID {
			return Project{}, "", nil, telegram.ErrNotMember
		}
		return p, ThreadClient, nil, nil
	case telegram.BotCreator:
		// Состав — тот же источник, что у кабинетной ручки креатора:
		// выведенный из проекта (removed_at) сюда не проходит, а
		// исполнитель общего проекта попадает в клиентскую ветку.
		thread, threadUser, err := b.s.repo.ResolveCreatorThread(ctx, projectID, userID)
		if errors.Is(err, ErrNotFound) {
			return Project{}, "", nil, telegram.ErrNotMember
		}
		if err != nil {
			return Project{}, "", nil, err
		}
		return p, thread, threadUser, nil
	default:
		return Project{}, "", nil, telegram.ErrUnknownBot
	}
}

// readerOf — кто прочтёт то, что этот человек пишет в эту ветку.
//
// Ветка заказчика обычно у менеджера. Исключение — общий проект: менеджера
// там нет, и ветку заказчика читают двое — заказчик и исполнитель. Тогда
// читатель — второй из них.
func readerOf(p Project, thread string, userID uuid.UUID) string {
	if thread != ThreadClient || p.Kind != KindGeneral {
		return telegram.ReaderManager
	}
	if p.SpecialistUserID != nil && *p.SpecialistUserID == userID {
		return telegram.ReaderClient
	}
	return telegram.ReaderExecutor
}

func projectRef(p Project, thread string, userID uuid.UUID) telegram.ProjectRef {
	return telegram.ProjectRef{
		ID: p.ID, Title: p.Title, Thread: thread,
		Reader: readerOf(p, thread, userID),
	}
}

func (b botComments) MemberProject(
	ctx context.Context, bot string, projectID, userID uuid.UUID,
) (telegram.ProjectRef, error) {
	p, thread, _, err := b.memberThread(ctx, bot, projectID, userID)
	if err != nil {
		return telegram.ProjectRef{}, err
	}
	return projectRef(p, thread, userID), nil
}

func (b botComments) CommentAsMember(
	ctx context.Context, bot string, projectID, userID uuid.UUID, text string,
) (telegram.ProjectRef, uuid.UUID, error) {
	// Ни пустоту, ни длину здесь не проверяем: это решает CreateComment,
	// один на кабинет и бота. Своя копия предела однажды разошлась бы с
	// ним, и бот обещал бы «записала» там, где кабинет отказал бы.
	p, thread, threadUser, err := b.memberThread(ctx, bot, projectID, userID)
	if err != nil {
		return telegram.ProjectRef{}, uuid.Nil, err
	}
	// Plain, а не html: из телеграма приходит текст, и разметку в нём
	// никто не ставил. Упоминаний тоже нет — их делают в кабинете.
	c, err := b.s.CreateComment(ctx, CommentRequest{
		ProjectID: p.ID, AuthorID: userID,
		Thread: thread, ThreadUserID: threadUser,
		Body: text, Format: FormatPlain,
	})
	switch {
	case errors.Is(err, ErrCommentEmpty):
		return telegram.ProjectRef{}, uuid.Nil, telegram.ErrCommentEmpty
	case errors.Is(err, ErrCommentTooLong):
		return telegram.ProjectRef{}, uuid.Nil, fmt.Errorf("%w: %v", telegram.ErrCommentTooLong, err)
	case errors.Is(err, ErrInvalidInput):
		// Прочий некорректный ввод — не «слишком длинно»: сказать
		// человеку «сократите» про то, что длиной не лечится, — хуже,
		// чем честное «не получилось».
		return telegram.ProjectRef{}, uuid.Nil, fmt.Errorf("%w: %v", telegram.ErrCommentInvalid, err)
	case err != nil:
		return telegram.ProjectRef{}, uuid.Nil, err
	}
	return projectRef(p, thread, userID), c.ID, nil
}
