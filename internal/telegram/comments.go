package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Ответ боту — комментарий в проекте.
//
// Пинг «завтра выкладка · Проект» человек читает в телеграме, и
// ответить на него естественно там же. Раньше такой ответ уходил в
// пустоту: бот отвечал справкой «работа живёт в кабинете», а вопрос
// креатора менеджер не видел вовсе. Теперь ответ ложится в переписку
// проекта — в ту ветку, куда человек написал бы из кабинета.
//
// Кто к какому проекту относится, знаем только мы: Telegram в ответе
// присылает лишь reply_to_message_id. Поэтому бот при квитировании
// сообщает, под каким message_id ушло каждое уведомление, и мы
// запоминаем пару «сообщение → проект» (таблица bot_sent_messages).
// Сам бот по-прежнему ничего не хранит.
//
// Комментарием становится ТОЛЬКО ответ (reply) на сообщение бота, которое
// мы запомнили: уведомление о проекте, приглашение «Написать в проект»
// или подтверждение «записала». Угадывать проект для простого сообщения
// («последний, о котором писали») не берёмся: у человека бывает два
// проекта сразу, и вопрос, ушедший не тому менеджеру, хуже честного
// «ответьте на сообщение о проекте».

var (
	// ErrNoProject — не поняли, к какому проекту это сообщение: ответ
	// на сообщение, которого нет в bot_sent_messages (справка, забытый
	// пинг, чужое), или проекта больше нет.
	ErrNoProject = errors.New("no project for this message")
	// ErrNotReply — сообщение не ответ ни на что. Проект не угадываем:
	// бот подсказывает, как написать в проект.
	ErrNotReply = errors.New("message is not a reply")
	// ErrNotMember — человек в проекте больше не участвует: выведен из
	// состава или проект чужой.
	ErrNotMember = errors.New("not a member of the project")
	// ErrCommentEmpty — пустой текст.
	ErrCommentEmpty = errors.New("comment is empty")
	// ErrCommentTooLong — длиннее, чем принимает переписка проекта.
	ErrCommentTooLong = errors.New("comment is too long")
	// ErrCommentInvalid — переписка отказала по другой причине ввода (не
	// длина и не пустота). Человеку — «не получилось», не «сократите».
	ErrCommentInvalid = errors.New("comment is invalid")
	// ErrCommentsDisabled — сервис собран без переписки проектов.
	ErrCommentsDisabled = errors.New("bot comments are not wired")
)

// ProjectRef — проект, в который ушёл комментарий.
type ProjectRef struct {
	ID    uuid.UUID `json:"project_id"`
	Title string    `json:"project_title"`
	// Thread — ветка: client (с заказчиком) или creator (с менеджером).
	// Бот по ней говорит, КТО прочитает: креатор общего проекта пишет
	// заказчику, а не менеджеру.
	Thread string `json:"thread"`
	// Reader — кто прочтёт: manager, client (заказчик) или executor
	// (исполнитель общего проекта). По нему бот говорит «менеджер
	// увидит» или «исполнитель увидит»; по ветке это не вывести: ветку
	// заказчика в общем проекте читает исполнитель, менеджера там нет.
	Reader string `json:"reader" enums:"manager,client,executor"`
}

// Кто прочтёт комментарий из бота.
const (
	ReaderManager  = "manager"
	ReaderClient   = "client"
	ReaderExecutor = "executor"
)

// ProjectComments — переписка проектов, как её видит бот.
//
// Реализует projects.Service (BotComments). Интерфейс здесь, а не там:
// projects уже импортирует этот пакет через auth, и обратная стрелка
// замкнула бы импорты в кольцо.
type ProjectComments interface {
	// MemberProject — человек всё ещё в проекте на стороне этого бота.
	MemberProject(ctx context.Context, bot string, projectID, userID uuid.UUID) (ProjectRef, error)
	// CommentAsMember — написать в его ветку тем же путём, что кабинет.
	CommentAsMember(ctx context.Context, bot string, projectID, userID uuid.UUID, text string) (ProjectRef, uuid.UUID, error)
}

// WithComments — подключить переписку проектов.
func (s *Service) WithComments(c ProjectComments) *Service {
	s.comments = c
	return s
}

// SentMessage — под каким message_id ушло сообщение очереди.
//
// Одно сообщение очереди — это одно событие и, возможно, несколько
// получателей, поэтому ключ — пара (id, chat_id).
type SentMessage struct {
	// ID — строка bot_messages.
	ID int64 `json:"id"`
	// ChatID — куда ушло.
	ChatID int64 `json:"chat_id"`
	// MessageID — message_id, который вернул Telegram.
	MessageID int64 `json:"message_id"`
}

// sentEnvelope — поля конверта, по которым отправленное сообщение
// привязывается к проекту и человеку.
type sentEnvelope struct {
	Audience  string `json:"audience"`
	Replyable bool   `json:"replyable"`
	Data      struct {
		ProjectID string `json:"project_id"`
	} `json:"data"`
	Recipients []Recipient `json:"recipients"`
}

// sentTarget — к какому проекту и человеку относится сообщение,
// ушедшее в chatID.
//
// Проект и человека берём из НАШЕГО конверта, а не из квитанции бота:
// бот сообщает только «куда и под каким номером ушло». Иначе ошибка
// или подмена на его стороне давала бы записать что угодно в чей
// угодно проект. Чат, которого нет среди получателей, не принимаем по
// той же причине.
func sentTarget(envelope []byte, chatID int64) (projectID, userID uuid.UUID, ok bool) {
	var env sentEnvelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	// Общий чат менеджеров: ответы там — разговор менеджеров между
	// собой, а не комментарии в проект.
	if env.Audience != "person" {
		return uuid.Nil, uuid.Nil, false
	}
	// Можно ли на это ответить в проект, решает таблица маршрутов
	// (eventroute.botRouting, флаг replyable): рассылка заявки тоже
	// несёт project_id, но получают её те, кто в проекте ещё не состоит.
	if !env.Replyable {
		return uuid.Nil, uuid.Nil, false
	}
	pid, err := uuid.Parse(env.Data.ProjectID)
	if err != nil || pid == uuid.Nil {
		// Заявки и приглашения проекта не имеют — отвечать на них
		// некуда, и это не ошибка.
		return uuid.Nil, uuid.Nil, false
	}
	for _, r := range env.Recipients {
		if r.TGChatID == chatID && r.UserID != uuid.Nil {
			return pid, r.UserID, true
		}
	}
	return uuid.Nil, uuid.Nil, false
}

// RememberSent — запомнить, к какому проекту относятся ушедшие
// уведомления.
//
// Строки без проекта (заявки, сообщения менеджерам) и чужие чаты молча
// пропускаем: квитанция их честно сообщает, а запоминать нечего.
// Проект, удалённый между отправкой и квитанцией, тоже пропускаем —
// WHERE EXISTS вместо падения на внешнем ключе.
func (r *Repo) RememberSent(ctx context.Context, sent []SentMessage) error {
	if len(sent) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(sent))
	for _, s := range sent {
		ids = append(ids, s.ID)
	}
	rows, err := r.db.Query(ctx,
		`SELECT id, bot, envelope FROM bot_messages WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("load sent envelopes: %w", err)
	}
	type row struct {
		bot      string
		envelope []byte
	}
	byID := make(map[int64]row, len(ids))
	for rows.Next() {
		var (
			id int64
			rw row
		)
		if err := rows.Scan(&id, &rw.bot, &rw.envelope); err != nil {
			rows.Close()
			return fmt.Errorf("scan sent envelope: %w", err)
		}
		byID[id] = rw
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sent envelopes: %w", err)
	}

	batch := &pgx.Batch{}
	for _, s := range sent {
		rw, ok := byID[s.ID]
		if !ok || s.ChatID == 0 || s.MessageID == 0 {
			continue
		}
		pid, uid, ok := sentTarget(rw.envelope, s.ChatID)
		if !ok {
			continue
		}
		batch.Queue(`
INSERT INTO bot_sent_messages (bot, chat_id, tg_message_id, project_id, user_id, bot_message_id)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM projects WHERE id = $4)
  AND EXISTS (SELECT 1 FROM users WHERE id = $5)
ON CONFLICT DO NOTHING`, rw.bot, s.ChatID, s.MessageID, pid, uid, s.ID)
	}
	if batch.Len() == 0 {
		return nil
	}
	if err := r.db.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("remember sent: %w", err)
	}
	return nil
}

// RememberAnchor — запомнить сообщение бота, написанное не из очереди:
// приглашение «напишите комментарий» и подтверждение «записала».
// Ответ на любое из них — тоже комментарий в этот проект.
func (r *Repo) RememberAnchor(
	ctx context.Context, bot string, chatID, messageID int64, projectID, userID uuid.UUID,
) error {
	_, err := r.db.Exec(ctx, `
INSERT INTO bot_sent_messages (bot, chat_id, tg_message_id, project_id, user_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (bot, chat_id, tg_message_id) DO UPDATE
   SET project_id = EXCLUDED.project_id, user_id = EXCLUDED.user_id`,
		bot, chatID, messageID, projectID, userID)
	if err != nil {
		return fmt.Errorf("remember anchor: %w", err)
	}
	return nil
}

// ProjectByMessage — к какому проекту относится сообщение бота.
//
// user_id в условии обязателен: строка записана для конкретного
// человека, и чужой ответ на неё в общем чате (если бота однажды
// добавят в группу) не должен писать от его имени.
func (r *Repo) ProjectByMessage(
	ctx context.Context, bot string, chatID, messageID int64, userID uuid.UUID,
) (uuid.UUID, error) {
	var pid uuid.UUID
	err := r.db.QueryRow(ctx, `
SELECT project_id FROM bot_sent_messages
WHERE bot = $1 AND chat_id = $2 AND tg_message_id = $3 AND user_id = $4`,
		bot, chatID, messageID, userID).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNoProject
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("project by message: %w", err)
	}
	return pid, nil
}

// CleanupSent — забыть старые соответствия.
//
// Ответ на пинг двухмесячной давности — редкость, а если и случится,
// получит вежливый отказ «не поняла, к какому проекту». Хранить каждое
// уведомление вечно ради этого незачем.
func (r *Repo) CleanupSent(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM bot_sent_messages WHERE created_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("cleanup bot sent messages: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---- сервис ----

// BotCommentInput — что прислал бот.
type BotCommentInput struct {
	Bot      string
	TGUserID int64
	ChatID   int64
	// ReplyTo — на какое сообщение бота человек ответил. 0 — не ответ,
	// и комментарием это не станет (ErrNotReply).
	ReplyTo int64
	Text    string
}

// BotCommentResult — что получилось.
type BotCommentResult struct {
	ProjectRef
	CommentID uuid.UUID `json:"comment_id"`
}

// linkedUser — кто пишет. Чат по умолчанию — из привязки: в личке он
// совпадает с телеграмом человека.
func (s *Service) linkedUser(ctx context.Context, bot string, tgUserID, chatID int64) (Link, int64, error) {
	if !KnownBot(bot) {
		return Link{}, 0, ErrUnknownBot
	}
	if s.comments == nil {
		return Link{}, 0, ErrCommentsDisabled
	}
	if tgUserID == 0 {
		return Link{}, 0, ErrNotFound
	}
	link, err := s.repo.ByTelegram(ctx, bot, tgUserID)
	if err != nil {
		return Link{}, 0, err
	}
	if chatID == 0 {
		chatID = link.TGChatID
	}
	return link, chatID, nil
}

// CommentFromBot — человек написал боту; превратить это в комментарий.
//
// Проект — только по ответу: reply на наше сообщение из
// bot_sent_messages — его проект. Ответ на сообщение, которого мы не
// помним, — ErrNoProject; не ответ вовсе — ErrNotReply. Бот обычно
// такие сообщения сюда и не шлёт (простой текст он сам встречает
// подсказкой), но правило держим здесь: бот старой версии шлёт всё.
func (s *Service) CommentFromBot(ctx context.Context, in BotCommentInput) (BotCommentResult, error) {
	link, chatID, err := s.linkedUser(ctx, in.Bot, in.TGUserID, in.ChatID)
	if err != nil {
		return BotCommentResult{}, err
	}
	if in.ReplyTo == 0 {
		return BotCommentResult{}, ErrNotReply
	}
	pid, err := s.repo.ProjectByMessage(ctx, in.Bot, chatID, in.ReplyTo, link.UserID)
	if err != nil {
		return BotCommentResult{}, err
	}
	ref, commentID, err := s.comments.CommentAsMember(ctx, in.Bot, pid, link.UserID, in.Text)
	if err != nil {
		return BotCommentResult{}, err
	}
	return BotCommentResult{ProjectRef: ref, CommentID: commentID}, nil
}

// CommentAnchor — проверить, что человек может писать в проект, и, если
// передан messageID, запомнить это сообщение бота как относящееся к
// проекту.
//
// Нужна кнопке «Написать в проект»: бот сперва спрашивает разрешения
// (и название — им подписано приглашение), потом отправляет
// приглашение и регистрирует его. Доступ проверяем на ОБОИХ шагах:
// между ними человека могли вывести из состава.
func (s *Service) CommentAnchor(
	ctx context.Context, bot string, tgUserID, chatID int64, projectID uuid.UUID, messageID int64,
) (ProjectRef, error) {
	link, chatID, err := s.linkedUser(ctx, bot, tgUserID, chatID)
	if err != nil {
		return ProjectRef{}, err
	}
	if projectID == uuid.Nil {
		return ProjectRef{}, ErrNoProject
	}
	ref, err := s.comments.MemberProject(ctx, bot, projectID, link.UserID)
	if err != nil {
		return ProjectRef{}, err
	}
	if messageID != 0 {
		if err := s.repo.RememberAnchor(ctx, bot, chatID, messageID, projectID, link.UserID); err != nil {
			return ProjectRef{}, err
		}
	}
	return ref, nil
}

// rememberSentQuietly — запомнить отправленное, не роняя квитанцию.
//
// Доставка важнее: сообщение уже у человека, и пометить его
// доставленным надо в любом случае — иначе через минуту оно уйдёт
// второй раз. Потерянное соответствие стоит меньше: ответ на этот
// пинг получит отказ «не поняла, к какому проекту», и человек напишет
// кнопкой «Написать в проект».
func (s *Service) rememberSentQuietly(ctx context.Context, sent []SentMessage) {
	if err := s.repo.RememberSent(ctx, sent); err != nil {
		slog.Warn("bot ack: соответствие сообщений проектам не записано",
			"err", err, "count", len(sent))
	}
}
