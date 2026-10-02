package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/eventroute"
	"marketpclce/internal/projects"
	"marketpclce/internal/telegram"
	"marketpclce/tests/integration"
)

// Ответ боту на пинг — комментарий в переписке проекта.
//
// Ломается это в двух направлениях, и оба тихие. Первое: ответ ушёл не
// в ту ветку — креатор пишет менеджеру, а читает заказчик, или наоборот.
// Второе: ответ ушёл от человека, которого из проекта уже вывели, — и
// бывший участник продолжает писать в чужую работу. Поэтому проверяем
// не «комментарий создан», а «создан там и от того».

// botStand — проект, сервис бота с подключённой перепиской и ручки.
type botStand struct {
	pool  *pgxpool.Pool
	repo  *telegram.Repo
	svc   *telegram.Service
	h     *telegram.Handler
	queue *telegram.Queue
}

func newBotStand(t *testing.T, pool *pgxpool.Pool) *botStand {
	t.Helper()
	repo := telegram.NewRepo(pool)
	svc := telegram.NewService(repo).
		WithBots("c_bot", "cl_bot", "s").
		WithComments(projects.NewService(projects.NewRepo(pool)).BotComments())
	return &botStand{
		pool: pool, repo: repo, svc: svc,
		h:     telegram.NewHandler(svc, nil),
		queue: telegram.NewQueue(pool),
	}
}

// tgID — свободный телеграм-id. Случайный, а не константа: привязки
// уникальны по (bot, tg_user_id), и соседний тест с той же константой
// получил бы ErrTaken.
func tgID() int64 { return 7_000_000_000 + rand.Int63n(1_000_000_000) }

func (s *botStand) link(t *testing.T, userID uuid.UUID, bot string) int64 {
	t.Helper()
	id := tgID()
	if _, err := s.repo.LinkDirect(context.Background(), userID, bot, id, id, "u", time.Now()); err != nil {
		t.Fatalf("LinkDirect: %v", err)
	}
	return id
}

// ping — положить уведомление в очередь и квитировать его через ручку
// с message_id, как это делает бот. Возвращает id строки очереди.
func (s *botStand) ping(
	t *testing.T, bot, audience, eventType string, projectID, userID uuid.UUID, chatID, messageID int64,
) int64 {
	t.Helper()
	ctx := context.Background()
	eventID := strconv.FormatInt(rand.Int63n(1<<40)+1<<41, 10)
	env := map[string]any{
		"bot": bot, "audience": audience, "event_type": eventType, "event_id": eventID,
		"data":      map[string]any{"project_id": projectID.String(), "project_title": "IT-проект"},
		"replyable": eventroute.Replyable(eventType),
	}
	if audience == "person" {
		env["recipients"] = []map[string]any{{"user_id": userID, "tg_chat_id": chatID}}
	}
	if err := s.queue.SendEnvelope(ctx, env); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM bot_messages WHERE event_id = $1::bigint`, eventID).Scan(&id); err != nil {
		t.Fatalf("строка очереди: %v", err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM bot_messages WHERE id = $1`, id) })

	code, _ := s.call(t, s.h.BotMessagesAck, map[string]any{
		"delivered": []int64{id},
		"failed":    []any{},
		"sent":      []map[string]any{{"id": id, "chat_id": chatID, "message_id": messageID}},
	})
	if code != http.StatusNoContent {
		t.Fatalf("ack: %d", code)
	}
	return id
}

func (s *botStand) call(t *testing.T, fn http.HandlerFunc, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	fn(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// commentRow — где и от кого лёг комментарий.
type commentRow struct {
	ProjectID    uuid.UUID
	AuthorID     uuid.UUID
	Thread       string
	ThreadUserID *uuid.UUID
	Body         string
}

func loadComment(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) commentRow {
	t.Helper()
	var c commentRow
	if err := pool.QueryRow(context.Background(), `
SELECT project_id, author_id, thread::text, thread_user_id, body_text
FROM project_comments WHERE id = $1`, id).
		Scan(&c.ProjectID, &c.AuthorID, &c.Thread, &c.ThreadUserID, &c.Body); err != nil {
		t.Fatalf("комментарий %s: %v", id, err)
	}
	return c
}

// Ответ креатора на пинг — в его ветку с менеджером, от его имени, с
// тем же событием, что у комментария из кабинета.
func TestBotReplyBecomesCreatorComment(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	assignManager(t, pool, pid)
	creator := addCreator(t, pool, pid, "Креатор из бота")

	s := newBotStand(t, pool)
	chat := s.link(t, creator, telegram.BotCreator)
	s.ping(t, telegram.BotCreator, "person", "project.publication_due_today", pid, creator, chat, 901)

	code, out := s.call(t, s.h.BotComment, map[string]any{
		"bot": "creator", "tg_user_id": chat, "chat_id": chat,
		"reply_to_message_id": 901, "text": "  Снимаю завтра, успею к вечеру  ",
	})
	if code != http.StatusCreated {
		t.Fatalf("ручка ответила %d: %v", code, out)
	}
	if out["project_title"] != "IT-проект" || out["thread"] != projects.ThreadCreator ||
		out["reader"] != telegram.ReaderManager {
		t.Errorf("ответ ручки: %v", out)
	}
	cid, err := uuid.Parse(out["comment_id"].(string))
	if err != nil {
		t.Fatalf("comment_id: %v", out["comment_id"])
	}
	c := loadComment(t, pool, cid)
	if c.ProjectID != pid || c.AuthorID != creator || c.Thread != projects.ThreadCreator ||
		c.ThreadUserID == nil || *c.ThreadUserID != creator {
		t.Errorf("комментарий лёг не туда: %+v", c)
	}
	if c.Body != "Снимаю завтра, успею к вечеру" {
		t.Errorf("текст: %q", c.Body)
	}

	// Менеджер узнаёт о нём тем же событием, что и о комментарии из
	// кабинета: своя вставка мимо CreateComment молча оставила бы его
	// без уведомления.
	var events int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM outbox
WHERE event_type = 'project.comment_added' AND payload->>'comment_id' = $1`,
		cid.String()).Scan(&events); err != nil {
		t.Fatalf("outbox: %v", err)
	}
	if events != 1 {
		t.Errorf("событий project.comment_added: %d, ожидали одно", events)
	}
}

// Комментарий — только ответ на запомненное сообщение бота. Ответ на
// старый пинг — в его проект, даже если с тех пор писали о другом;
// простое сообщение и ответ на неизвестное сообщение проект не угадывают.
func TestBotCommentOnlyByReply(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, _, older, cleanupA := setupPipelineAndProject(t, pool)
	defer cleanupA()
	_, _, newer, cleanupB := setupPipelineAndProject(t, pool)
	defer cleanupB()
	creator := addCreator(t, pool, older, "Креатор двух проектов")
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_creators (project_id, creator_user_id) VALUES ($1, $2)`,
		newer, creator); err != nil {
		t.Fatalf("второй проект: %v", err)
	}

	s := newBotStand(t, pool)
	chat := s.link(t, creator, telegram.BotCreator)
	s.ping(t, telegram.BotCreator, "person", "project.publication_due_tomorrow", older, creator, chat, 10)
	s.ping(t, telegram.BotCreator, "person", "project.publication_due_today", newer, creator, chat, 11)

	t.Run("простое сообщение — не комментарий", func(t *testing.T) {
		if _, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
			Bot: telegram.BotCreator, TGUserID: chat, ChatID: chat, Text: "просто вопрос",
		}); !errors.Is(err, telegram.ErrNotReply) {
			t.Errorf("простое сообщение: %v, ожидали ErrNotReply", err)
		}
		// Бот старой версии шлёт и простой текст — ручка отказывает
		// своей причиной, по ней бот покажет подсказку.
		code, out := s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": chat, "chat_id": chat, "text": "просто вопрос",
		})
		if code != http.StatusBadRequest || out["error"] != "not_reply" {
			t.Errorf("ручка на простое сообщение: %d %v", code, out)
		}
	})

	t.Run("ответ на неизвестное сообщение — отказ", func(t *testing.T) {
		if _, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
			Bot: telegram.BotCreator, TGUserID: chat, ChatID: chat, ReplyTo: 999, Text: "на справку",
		}); !errors.Is(err, telegram.ErrNoProject) {
			t.Errorf("ответ на неизвестное сообщение: %v, ожидали ErrNoProject", err)
		}
		code, out := s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": chat, "chat_id": chat,
			"reply_to_message_id": 999, "text": "на справку",
		})
		if code != http.StatusNotFound || out["error"] != "no_project" {
			t.Errorf("ручка на неизвестное сообщение: %d %v", code, out)
		}
	})

	t.Run("ответ на пинг — в его проект", func(t *testing.T) {
		res, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
			Bot: telegram.BotCreator, TGUserID: chat, ChatID: chat, ReplyTo: 10, Text: "про старый пинг",
		})
		if err != nil {
			t.Fatalf("ответ на старый пинг: %v", err)
		}
		if res.ID != older {
			t.Errorf("ответ на пинг первого проекта ушёл в %s", res.ID)
		}
		if c := loadComment(t, pool, res.CommentID); c.ProjectID != older {
			t.Errorf("в базе: %+v", c)
		}
		// Давность не важна: явный ответ понятен и через трое суток.
		if _, err := pool.Exec(ctx, `
UPDATE bot_sent_messages SET created_at = now() - interval '10 days'
WHERE chat_id = $1`, chat); err != nil {
			t.Fatalf("состарить: %v", err)
		}
		if res, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
			Bot: telegram.BotCreator, TGUserID: chat, ChatID: chat, ReplyTo: 11, Text: "поздний ответ",
		}); err != nil || res.ID != newer {
			t.Errorf("явный ответ на старый пинг: %+v, %v", res, err)
		}
	})

	// В проектах — ровно два комментария: от двух ответов. Отказы
	// ничего не записали и никуда не «угадали».
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_comments WHERE project_id = ANY($1)`,
		[]uuid.UUID{older, newer}).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("комментариев в проектах: %d, ожидали 2", n)
	}
}

// Отказы: выведенный из состава, посторонний, без привязки, пустое и
// слишком длинное.
func TestBotCommentRefusals(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	creator := addCreator(t, pool, pid, "Креатор")
	stranger := addCreator(t, pool, pid, "Посторонний")
	if _, err := pool.Exec(ctx,
		`DELETE FROM project_creators WHERE project_id = $1 AND creator_user_id = $2`,
		pid, stranger); err != nil {
		t.Fatalf("убрать постороннего: %v", err)
	}

	s := newBotStand(t, pool)
	chat := s.link(t, creator, telegram.BotCreator)
	s.ping(t, telegram.BotCreator, "person", "project.publication_due_today", pid, creator, chat, 50)

	t.Run("пустой текст", func(t *testing.T) {
		code, out := s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": chat, "reply_to_message_id": 50, "text": "   ",
		})
		if code != http.StatusBadRequest || out["error"] != "empty_comment" {
			t.Errorf("%d %v", code, out)
		}
	})
	t.Run("слишком длинный", func(t *testing.T) {
		code, out := s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": chat, "reply_to_message_id": 50,
			"text": strings.Repeat("я", 5001),
		})
		if code != http.StatusBadRequest || out["error"] != "too_long" {
			t.Errorf("%d %v", code, out)
		}
	})
	t.Run("без привязки", func(t *testing.T) {
		code, out := s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": tgID(), "text": "кто я",
		})
		if code != http.StatusNotFound || out["error"] != "not_linked" {
			t.Errorf("%d %v", code, out)
		}
	})
	t.Run("посторонний", func(t *testing.T) {
		// Чужого пинга у него нет, а кнопку с чужим проектом можно
		// подделать: доступ проверяет API, а не бот.
		strangerChat := s.link(t, stranger, telegram.BotCreator)
		code, out := s.call(t, s.h.BotCommentAnchor, map[string]any{
			"bot": "creator", "tg_user_id": strangerChat, "chat_id": strangerChat,
			"project_id": pid.String(), "message_id": 77,
		})
		if code != http.StatusForbidden || out["error"] != "not_member" {
			t.Errorf("приглашение постороннему: %d %v", code, out)
		}
		code, out = s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": strangerChat, "reply_to_message_id": 77, "text": "влезу",
		})
		if code != http.StatusNotFound || out["error"] != "no_project" {
			t.Errorf("сообщение постороннего: %d %v", code, out)
		}
		// И пинг креатора чужому не помогает: соответствие записано
		// на того, кому писали.
		if _, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
			Bot: telegram.BotCreator, TGUserID: strangerChat, ChatID: chat, ReplyTo: 50, Text: "чужой ответ",
		}); !errors.Is(err, telegram.ErrNoProject) {
			t.Errorf("ответ на чужой пинг: %v", err)
		}
	})
	t.Run("выведен из состава", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
UPDATE project_creators SET removed_at = now()
WHERE project_id = $1 AND creator_user_id = $2`, pid, creator); err != nil {
			t.Fatalf("вывести: %v", err)
		}
		code, out := s.call(t, s.h.BotComment, map[string]any{
			"bot": "creator", "tg_user_id": chat, "reply_to_message_id": 50, "text": "я ещё тут?",
		})
		if code != http.StatusForbidden || out["error"] != "not_member" {
			t.Errorf("%d %v", code, out)
		}
	})

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_comments WHERE project_id = $1`, pid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("после одних отказов в проекте %d комментариев", n)
	}
}

// Заказчик из клиентского бота — в ветку с заказчиком. И только он:
// креатор того же проекта клиентским ботом в неё не напишет.
func TestBotClientReplyGoesToClientThread(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	creator := addCreator(t, pool, pid, "Креатор")

	s := newBotStand(t, pool)
	chat := s.link(t, clientID, telegram.BotClient)
	s.ping(t, telegram.BotClient, "person", "project.client_new_video", pid, clientID, chat, 300)

	res, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
		Bot: telegram.BotClient, TGUserID: chat, ChatID: chat, ReplyTo: 300, Text: "Отличный ролик",
	})
	if err != nil {
		t.Fatalf("CommentFromBot: %v", err)
	}
	if res.Reader != telegram.ReaderManager {
		t.Errorf("ветку заказчика обычного проекта читает менеджер, а не %q", res.Reader)
	}
	c := loadComment(t, pool, res.CommentID)
	if c.Thread != projects.ThreadClient || c.ThreadUserID != nil || c.AuthorID != clientID || c.ProjectID != pid {
		t.Errorf("комментарий заказчика лёг не туда: %+v", c)
	}

	creatorChat := s.link(t, creator, telegram.BotClient)
	if _, err := s.svc.CommentAnchor(ctx, telegram.BotClient, creatorChat, creatorChat, pid, 0); !errors.Is(err, telegram.ErrNotMember) {
		t.Errorf("креатор через клиентский бот: %v", err)
	}
}

// Общий проект: менеджера нет, ветку заказчика читают двое — заказчик
// и исполнитель. Бот должен говорить «исполнитель увидит» заказчику и
// «заказчик увидит» исполнителю, а не «менеджер увидит».
func TestBotGeneralProjectReader(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()
	p, err := projects.NewService(projects.NewRepo(pool)).CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("CreateGeneral: %v", err)
	}

	s := newBotStand(t, pool)
	clientChat := s.link(t, clientID, telegram.BotClient)
	specChat := s.link(t, specID, telegram.BotCreator)
	s.ping(t, telegram.BotClient, "person", "project.client_new_video", p.ID, clientID, clientChat, 21)
	s.ping(t, telegram.BotCreator, "person", "project.publication_due_today", p.ID, specID, specChat, 22)

	code, out := s.call(t, s.h.BotComment, map[string]any{
		"bot": "client", "tg_user_id": clientChat, "chat_id": clientChat,
		"reply_to_message_id": 21, "text": "Как успехи?",
	})
	if code != http.StatusCreated || out["thread"] != projects.ThreadClient ||
		out["reader"] != telegram.ReaderExecutor {
		t.Errorf("заказчик общего проекта: %d %v", code, out)
	}

	res, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
		Bot: telegram.BotCreator, TGUserID: specChat, ChatID: specChat, ReplyTo: 22, Text: "Почти готово",
	})
	if err != nil {
		t.Fatalf("исполнитель общего проекта: %v", err)
	}
	if res.Thread != projects.ThreadClient || res.Reader != telegram.ReaderClient {
		t.Errorf("исполнитель общего проекта: %+v", res)
	}

	// Кнопка «Написать в проект» говорит о читателе то же самое.
	ref, err := s.svc.CommentAnchor(ctx, telegram.BotClient, clientChat, clientChat, p.ID, 0)
	if err != nil || ref.Reader != telegram.ReaderExecutor {
		t.Errorf("проверка по кнопке: %+v, %v", ref, err)
	}
}

// Кнопка «Написать в проект»: проверка, приглашение, ответ на него.
// Плюс: квитанция по сообщению в чат менеджеров ничего не запоминает.
func TestBotWriteButtonFlow(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	creator := addCreator(t, pool, pid, "Креатор")

	s := newBotStand(t, pool)
	chat := s.link(t, creator, telegram.BotCreator)

	// Шаг 1: нажали кнопку — только проверка и название.
	code, out := s.call(t, s.h.BotCommentAnchor, map[string]any{
		"bot": "creator", "tg_user_id": chat, "chat_id": chat, "project_id": pid.String(),
	})
	if code != http.StatusOK || out["project_title"] != "IT-проект" {
		t.Fatalf("проверка: %d %v", code, out)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM bot_sent_messages WHERE chat_id = $1`, chat).Scan(&rows)
	if rows != 0 {
		t.Errorf("проверка без message_id что-то записала: %d", rows)
	}

	// Шаг 2: приглашение отправлено — регистрируем.
	if _, err := s.svc.CommentAnchor(ctx, telegram.BotCreator, chat, chat, pid, 4242); err != nil {
		t.Fatalf("регистрация приглашения: %v", err)
	}
	// Шаг 3: ответ на приглашение.
	res, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
		Bot: telegram.BotCreator, TGUserID: chat, ChatID: chat, ReplyTo: 4242, Text: "по кнопке",
	})
	if err != nil || res.ID != pid {
		t.Fatalf("ответ на приглашение: %+v, %v", res, err)
	}

	// Сообщение в общий чат менеджеров: ответы там — разговор
	// менеджеров, а не комментарии. Квитанция с message_id ничего не
	// запоминает.
	managersChat := -1000000000000 - rand.Int63n(1000)
	s.ping(t, telegram.BotCreator, "managers", "project.publication_overdue", pid, uuid.Nil, managersChat, 5)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM bot_sent_messages WHERE chat_id = $1`, managersChat).Scan(&rows)
	if rows != 0 {
		t.Errorf("сообщение менеджерам запомнено как личное: %d", rows)
	}
}

// Событие ушло одному адресату и не ушло другому: строка очереди
// квитирована как failed, но message_id успешного бот всё равно
// присылает в sent — и ответ на это сообщение становится комментарием.
// Повторная квитанция с тем же номером (повтор доставки) ничего не
// ломает.
func TestBotPartialAckRemembersSent(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()
	creator := addCreator(t, pool, pid, "Креатор")
	other := addCreator(t, pool, pid, "Второй креатор")

	s := newBotStand(t, pool)
	chat := s.link(t, creator, telegram.BotCreator)
	otherChat := s.link(t, other, telegram.BotCreator)

	eventID := strconv.FormatInt(rand.Int63n(1<<40)+1<<41, 10)
	if err := s.queue.SendEnvelope(ctx, map[string]any{
		"bot": "creator", "audience": "person", "event_type": "project.publication_due_today",
		"event_id": eventID, "replyable": true,
		"data": map[string]any{"project_id": pid.String(), "project_title": "IT-проект"},
		"recipients": []map[string]any{
			{"user_id": creator, "tg_chat_id": chat},
			{"user_id": other, "tg_chat_id": otherChat},
		},
	}); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM bot_messages WHERE event_id = $1::bigint`, eventID).Scan(&id); err != nil {
		t.Fatalf("строка очереди: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM bot_messages WHERE id = $1`, id) })

	ack := map[string]any{
		"delivered": []int64{},
		"failed":    []map[string]any{{"id": id, "error": "telegram 502"}},
		"sent":      []map[string]any{{"id": id, "chat_id": chat, "message_id": 61}},
	}
	for i := 0; i < 2; i++ {
		if code, out := s.call(t, s.h.BotMessagesAck, ack); code != http.StatusNoContent {
			t.Fatalf("ack #%d: %d %v", i+1, code, out)
		}
	}

	res, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
		Bot: telegram.BotCreator, TGUserID: chat, ChatID: chat, ReplyTo: 61, Text: "получил",
	})
	if err != nil || res.ID != pid {
		t.Fatalf("ответ на сообщение из частично доставленного события: %+v, %v", res, err)
	}
	// Второму не ушло — и запоминать за него нечего.
	if _, err := s.svc.CommentFromBot(ctx, telegram.BotCommentInput{
		Bot: telegram.BotCreator, TGUserID: otherChat, ChatID: otherChat, ReplyTo: 61, Text: "а я?",
	}); !errors.Is(err, telegram.ErrNoProject) {
		t.Errorf("недоставленный адресат: %v", err)
	}
}
