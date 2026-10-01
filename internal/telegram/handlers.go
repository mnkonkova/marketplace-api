package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Ручки привязки: кабинет (человек подключает бота) и бот (бот
// сообщает, что человек нажал /start или заблокировал его).
//
// Разные группы и разные способы авторизации: кабинет — по JWT
// человека, бот — по общему секрету. Смешивать нельзя: у бота нет
// пользователя, а у человека нет права привязать чужой телеграм.

// UserIDFunc — как достать человека из контекста запроса.
//
// Функцией, а не импортом auth: auth уже импортирует этот пакет ради
// проверки подписи мини-аппа, и обратная стрелка замкнула бы импорты в
// кольцо. Подставляется одной строкой при сборке роутера.
type UserIDFunc func(ctx context.Context) (uuid.UUID, bool)

type Handler struct {
	svc    *Service
	userID UserIDFunc
}

func NewHandler(svc *Service, userID UserIDFunc) *Handler {
	return &Handler{svc: svc, userID: userID}
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownBot):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "unknown_bot",
			"Неизвестный бот: бывают creator и client.")
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Привязка не найдена.")
	case errors.Is(err, ErrCodeExpired):
		httpx.WriteErrMsg(w, http.StatusGone, "code_expired",
			"Ссылка устарела — получите новую в кабинете.")
	case errors.Is(err, ErrTaken):
		httpx.WriteErrMsg(w, http.StatusConflict, "telegram_taken",
			"Этот телеграм уже привязан к другому аккаунту.")
	default:
		slog.Error("telegram handler", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// ---- кабинет ----

// MyStatus godoc
// @Summary  Мои привязки к ботам
// @Tags     telegram
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} Status
// @Failure  401 {object} errorResponse "no_user"
// @Router   /me/telegram [get]
func (h *Handler) MyStatus(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	out, err := h.svc.Status(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type linkCodeReq struct {
	Bot string `json:"bot"`
}

// LinkCode godoc
// @Summary  Ссылка подключения бота
// @Description Одноразовый код внутри ссылки живёт 15 минут. Прежние
// @Description неиспользованные коды этого человека гасятся: две живые
// @Description ссылки на один аккаунт — это две двери, а нужна одна.
// @Tags     telegram
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body linkCodeReq true "бот"
// @Success  200 {object} LinkStart
// @Failure  400 {object} errorResponse "unknown_bot — бот не настроен"
// @Failure  401 {object} errorResponse "no_user"
// @Router   /me/telegram/link-code [post]
func (h *Handler) LinkCode(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in linkCodeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	out, err := h.svc.NewLinkStart(r.Context(), uid, strings.TrimSpace(in.Bot))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type claimReq struct {
	Code string `json:"code"`
}

// Claim godoc
// @Summary  Привязать телеграм по билету из мини-аппа
// @Description Человек нажал «я здесь впервые» в боте, зарегистрировался в
// @Description браузере — и телеграм привязывается сам, без возврата в бот.
// @Description Билет доказывает «этот телеграм просил привязку», сессия —
// @Description «это его аккаунт»; по отдельности ни того, ни другого мало.
// @Tags     telegram
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body claimReq true "билет"
// @Success  200 {object} Link
// @Failure  400 {object} errorResponse "bad_json"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — билета нет"
// @Failure  409 {object} errorResponse "telegram_taken"
// @Failure  410 {object} errorResponse "code_expired"
// @Router   /me/telegram/claim [post]
func (h *Handler) Claim(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in claimReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	link, err := h.svc.Claim(r.Context(), uid, strings.TrimSpace(in.Code))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, link)
}

// Unlink godoc
// @Summary  Отключить бота
// @Tags     telegram
// @Produce  json
// @Security BearerAuth
// @Param    bot query string true "creator | client"
// @Success  204
// @Failure  400 {object} errorResponse "unknown_bot"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found"
// @Router   /me/telegram/link [delete]
func (h *Handler) Unlink(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userID(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	if err := h.svc.Unlink(r.Context(), uid, strings.TrimSpace(r.URL.Query().Get("bot"))); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- бот → мы ----

// RequireSecret — middleware группы /bot/*: общий секрет в
// Authorization: Bearer.
//
// Отдельный от JWT способ намеренно: у бота нет пользователя, а
// раздавать ему сервисный аккаунт значит завести учётку, которой
// можно ходить куда угодно.
func (h *Handler) RequireSecret(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		if !h.svc.CheckSecret(got) {
			httpx.WriteErrMsg(w, http.StatusUnauthorized, "bad_secret",
				"Неверный секрет сервиса бота.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type botLinkReq struct {
	Bot        string `json:"bot"`
	Code       string `json:"code"`
	TGUserID   int64  `json:"tg_user_id"`
	TGChatID   int64  `json:"tg_chat_id"`
	TGUsername string `json:"tg_username"`
}

// BotLink godoc
// @Summary  Привязать телеграм по коду (бот)
// @Description Человек нажал /start с кодом из кабинета. Код гасится, и
// @Description привязка появляется в той же транзакции: погашенный код без
// @Description привязки — это человек без уведомлений, и по логам этого не
// @Description видно.
// @Tags     bot
// @Accept   json
// @Produce  json
// @Param    body body botLinkReq true "код и телеграм"
// @Success  200 {object} Link
// @Failure  400 {object} errorResponse "bad_json; unknown_bot"
// @Failure  401 {object} errorResponse "bad_secret"
// @Failure  404 {object} errorResponse "not_found — кода нет"
// @Failure  409 {object} errorResponse "telegram_taken"
// @Failure  410 {object} errorResponse "code_expired — код протух или уже использован"
// @Router   /bot/link [post]
func (h *Handler) BotLink(w http.ResponseWriter, r *http.Request) {
	var in botLinkReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	link, err := h.svc.LinkByCode(r.Context(), strings.TrimSpace(in.Bot),
		strings.TrimSpace(in.Code), in.TGUserID, in.TGChatID, strings.TrimSpace(in.TGUsername))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, link)
}

// BotWhoIs godoc
// @Summary  Кто это (бот)
// @Description Бот знает только tg_user_id. Без этой ручки он на каждое
// @Description сообщение не понимает, с кем говорит.
// @Tags     bot
// @Produce  json
// @Param    tg_user_id path int true "telegram user id"
// @Param    bot query string true "creator | client"
// @Success  200 {object} Link
// @Failure  400 {object} errorResponse "bad_id; unknown_bot"
// @Failure  401 {object} errorResponse "bad_secret"
// @Failure  404 {object} errorResponse "not_linked"
// @Router   /bot/users/by-telegram/{tg_user_id} [get]
func (h *Handler) BotWhoIs(w http.ResponseWriter, r *http.Request) {
	tgID, err := strconv.ParseInt(chi.URLParam(r, "tg_user_id"), 10, 64)
	if err != nil || tgID == 0 {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный telegram id.")
		return
	}
	bot := strings.TrimSpace(r.URL.Query().Get("bot"))
	if !KnownBot(bot) {
		writeErr(w, ErrUnknownBot)
		return
	}
	link, err := h.svc.ByTelegram(r.Context(), bot, tgID)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_linked",
			"Этот телеграм ни к кому не привязан.")
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, link)
}

type botBlockedReq struct {
	Bot      string `json:"bot"`
	TGUserID int64  `json:"tg_user_id"`
}

// BotBlocked godoc
// @Summary  Человек заблокировал бота
// @Description Строку привязки не удаляем: удаление выглядит как «никогда не
// @Description подключал», и мы бы позвали его подключиться заново — то есть
// @Description предложили то, от чего он только что отказался.
// @Tags     bot
// @Accept   json
// @Produce  json
// @Param    body body botBlockedReq true "бот и телеграм"
// @Success  200 {object} Link
// @Failure  400 {object} errorResponse "bad_json; unknown_bot"
// @Failure  401 {object} errorResponse "bad_secret"
// @Failure  404 {object} errorResponse "not_found"
// @Router   /bot/blocked [post]
func (h *Handler) BotBlocked(w http.ResponseWriter, r *http.Request) {
	var in botBlockedReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	if !KnownBot(strings.TrimSpace(in.Bot)) {
		writeErr(w, ErrUnknownBot)
		return
	}
	link, err := h.svc.Blocked(r.Context(), strings.TrimSpace(in.Bot), in.TGUserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, link)
}

// ---- очередь сообщений ----
//
// Доставка перевёрнута: не мы стучимся в сервис бота, а он забирает
// готовые сообщения сам. Причина — в queue.go; коротко: наша ВДС не
// обязана уметь дозваниваться до чужого облака, а обратное
// направление работает всегда.

type botMessagesResp struct {
	Items []Message `json:"items"`
}

// BotMessages godoc
// @Summary  Забрать сообщения для отправки (бот)
// @Description Выдаёт самые старые неотправленные и берёт их в аренду:
// @Description пока бот не подтвердил доставку, сообщение остаётся в очереди
// @Description и через минуту достанется следующему опросу. Потерять
// @Description сообщение из-за упавшего контейнера нельзя; отправить дважды —
// @Description можно пережить, на стороне бота стоит дедуп по event_id.
// @Tags     bot
// @Produce  json
// @Param    limit query int false "сколько отдать, по умолчанию и максимум — 20"
// @Success  200 {object} botMessagesResp
// @Failure  401 {object} errorResponse "bad_secret"
// @Router   /bot/messages [get]
func (h *Handler) BotMessages(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.svc.LeaseMessages(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, botMessagesResp{Items: items})
}

type botAckReq struct {
	// Delivered — ушло в телеграм.
	Delivered []int64 `json:"delivered"`
	// Failed — не ушло и почему. Такие возвращаются в очередь: причина
	// пишется в строку, следующий опрос возьмёт их снова.
	Failed []struct {
		ID    int64  `json:"id"`
		Error string `json:"error"`
	} `json:"failed"`
}

// BotMessagesAck godoc
// @Summary  Подтвердить доставку (бот)
// @Tags     bot
// @Accept   json
// @Produce  json
// @Param    body body botAckReq true "что доставлено, что нет"
// @Success  204
// @Failure  400 {object} errorResponse "bad_json"
// @Failure  401 {object} errorResponse "bad_secret"
// @Router   /bot/messages/ack [post]
func (h *Handler) BotMessagesAck(w http.ResponseWriter, r *http.Request) {
	var in botAckReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	failed := make(map[int64]string, len(in.Failed))
	for _, f := range in.Failed {
		failed[f.ID] = f.Error
	}
	if err := h.svc.AckMessages(r.Context(), in.Delivered, failed); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
