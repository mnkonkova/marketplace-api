package telegram

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Ручки «ответ боту → комментарий в проекте». Под тем же общим
// секретом, что и остальные /bot/*: бот говорит от имени человека,
// которого опознал по привязке, и только мы решаем, куда ему можно
// писать.

type botCommentReq struct {
	Bot      string `json:"bot"`
	TGUserID int64  `json:"tg_user_id"`
	ChatID   int64  `json:"chat_id"`
	// ReplyToMessageID — на какое сообщение бота человек ответил.
	// Обязательно: не ответ — не комментарий (not_reply).
	ReplyToMessageID int64  `json:"reply_to_message_id,omitempty"`
	Text             string `json:"text"`
}

// writeCommentErr — «не привязан» здесь отдельная причина, а не общее
// «привязка не найдена»: бот отвечает на неё приглашением подключиться.
func writeCommentErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_linked",
			"Этот телеграм ни к кому не привязан.")
		return
	}
	writeErr(w, err)
}

// BotComment godoc
// @Summary  Ответ боту — комментарий в проект (бот)
// @Description Человек ответил (reply) на сообщение бота о проекте —
// @Description текст ложится в переписку проекта от его имени, в ту ветку,
// @Description куда он написал бы из кабинета: креатор — в свою ветку с
// @Description менеджером (исполнитель общего проекта — в ветку с
// @Description заказчиком), заказчик — в ветку с заказчиком. Уведомления
// @Description менеджеру уходят те же, что у комментария из кабинета.
// @Description Проект — только по reply_to_message_id: сообщение бота о
// @Description проекте (уведомление, приглашение «Написать в проект»,
// @Description подтверждение «записала»), запомненное API. Ответ на
// @Description неизвестное сообщение — no_project; не ответ — not_reply.
// @Description Проект не угадывается. В ответе reader — кто прочтёт:
// @Description manager, client (заказчик) или executor (исполнитель общего проекта).
// @Tags     bot
// @Accept   json
// @Produce  json
// @Param    body body botCommentReq true "кто, где и что написал"
// @Success  201 {object} BotCommentResult
// @Failure  400 {object} errorResponse "bad_json; unknown_bot; not_reply — не ответ на сообщение бота; empty_comment; too_long — длиннее 5000 знаков; invalid_comment — прочий некорректный ввод"
// @Failure  401 {object} errorResponse "bad_secret"
// @Failure  403 {object} errorResponse "not_member — человек выведен из состава или проект чужой"
// @Failure  404 {object} errorResponse "not_linked — телеграм не привязан; no_project — ответ на сообщение, которого API не знает, или проекта нет"
// @Failure  503 {object} errorResponse "comments_disabled"
// @Router   /bot/comments [post]
func (h *Handler) BotComment(w http.ResponseWriter, r *http.Request) {
	var in botCommentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	out, err := h.svc.CommentFromBot(r.Context(), BotCommentInput{
		Bot:      strings.TrimSpace(in.Bot),
		TGUserID: in.TGUserID,
		ChatID:   in.ChatID,
		ReplyTo:  in.ReplyToMessageID,
		Text:     in.Text,
	})
	if err != nil {
		writeCommentErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

type botCommentAnchorReq struct {
	Bot       string `json:"bot"`
	TGUserID  int64  `json:"tg_user_id"`
	ChatID    int64  `json:"chat_id"`
	ProjectID string `json:"project_id"`
	// MessageID — сообщение бота, ответ на которое пойдёт в этот
	// проект. Пусто — только проверить доступ и узнать название.
	MessageID int64 `json:"message_id,omitempty"`
}

// BotCommentAnchor godoc
// @Summary  Привязать сообщение бота к проекту (бот)
// @Description Для кнопки «Написать в проект». Без message_id — проверка:
// @Description человек всё ещё в проекте, и как проект называется (им
// @Description подписано приглашение). С message_id — то же плюс «ответ на
// @Description это сообщение — комментарий в этот проект»: так бот
// @Description регистрирует приглашение и подтверждение «записала». Доступ
// @Description проверяется каждый раз: между шагами человека могли вывести.
// @Tags     bot
// @Accept   json
// @Produce  json
// @Param    body body botCommentAnchorReq true "кто, какой проект, какое сообщение"
// @Success  200 {object} ProjectRef
// @Failure  400 {object} errorResponse "bad_json; unknown_bot"
// @Failure  401 {object} errorResponse "bad_secret"
// @Failure  403 {object} errorResponse "not_member"
// @Failure  404 {object} errorResponse "not_linked; no_project — проекта нет"
// @Failure  503 {object} errorResponse "comments_disabled"
// @Router   /bot/comments/anchor [post]
func (h *Handler) BotCommentAnchor(w http.ResponseWriter, r *http.Request) {
	var in botCommentAnchorReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	// Битый id — тот же «нет такого проекта»: кнопку рисовали мы, и
	// мусор в ней — не повод для отдельной причины.
	pid, _ := uuid.Parse(strings.TrimSpace(in.ProjectID))
	ref, err := h.svc.CommentAnchor(r.Context(), strings.TrimSpace(in.Bot),
		in.TGUserID, in.ChatID, pid, in.MessageID)
	if err != nil {
		writeCommentErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ref)
}
