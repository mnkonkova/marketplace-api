package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// commentReq — тело запроса на создание сообщения.
//
// thread/creator_id заполняет только персонал: клиент и креатор пишут каждый
// в свою ветку, и позволить им выбирать её значило бы дать написать в чужую.
type commentReq struct {
	Body string `json:"body"`
	// Format — plain (по умолчанию) или html. Для html разметка чистится на
	// сервере: см. internal/richtext.
	Format string `json:"body_format"`
	// IsInternal — прежнее поле. Оставлено для уже написанного фронта:
	// true равнозначно thread="internal".
	IsInternal bool `json:"is_internal"`
	// Thread — client | creator | internal. Пусто = client (или internal,
	// если выставлен is_internal).
	Thread string `json:"thread"`
	// CreatorID — чья креаторская ветка. Обязателен при thread="creator".
	CreatorID string `json:"creator_id"`
}

// thread разбирает ветку из тела запроса персонала.
func (in commentReq) thread() (string, *uuid.UUID, error) {
	t := in.Thread
	if t == "" {
		t = ThreadClient
		if in.IsInternal {
			t = ThreadInternal
		}
	}
	if t != ThreadCreator {
		return t, nil, nil
	}
	id, err := uuid.Parse(in.CreatorID)
	if err != nil {
		return "", nil, errBadCreatorID
	}
	return t, &id, nil
}

var errBadCreatorID = errors.New("creator_id is not a uuid")

// staffThread — разобрать ветку и убедиться, что креаторская принадлежит
// креатору ЭТОГО проекта. Без проверки менеджер мог бы завести переписку с
// посторонним пользователем внутри чужого проекта.
func (h *Handler) staffThread(w http.ResponseWriter, r *http.Request, pid uuid.UUID, in commentReq) (string, *uuid.UUID, bool) {
	thread, creatorID, err := in.thread()
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_creator_id",
			"creator_id должен быть корректным uuid.")
		return "", nil, false
	}
	if thread == ThreadCreator {
		got, _, err := h.svc.ResolveCreatorThread(r.Context(), pid, *creatorID)
		if err != nil || got != ThreadCreator {
			httpx.WriteErrMsg(w, http.StatusNotFound, "not_a_project_creator",
				"Этот пользователь не в составе проекта.")
			return "", nil, false
		}
	}
	return thread, creatorID, true
}

// ---- Client: своя ветка с менеджером ----

// ClientCreateComment godoc
// @Summary  Клиент пишет в переписку по своему проекту
// @Description Сообщение уходит в клиентскую ветку. Креаторы её не видят.
// @Description body_format=html — разметка чистится на сервере: остаются
// @Description жирный, курсив, списки, ссылки и упоминания, остальное
// @Description выбрасывается. Упомянуть можно только участника ветки.
// @Tags     client-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    body body commentReq true "body"
// @Success  201  {object} Comment
// @Failure  400  {object} errorResponse "bad_json; bad_id; empty_comment — пустое сообщение; invalid_input — превышена длина или неизвестный body_format"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или чужой"
// @Router   /me/projects/{id}/comments [post]
func (h *Handler) ClientCreateComment(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	// Владение — через GetClientProject (фильтр client_user_id).
	if _, err := h.svc.GetClientProject(r.Context(), pid, uid); err != nil {
		writeServiceErr(w, err)
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	// Ветку клиент не выбирает: его ветка одна.
	c, err := h.svc.CreateComment(r.Context(), CommentRequest{
		ProjectID: pid, AuthorID: uid, Thread: ThreadClient,
		Body: in.Body, Format: in.Format,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// ClientListComments godoc
// @Summary  Переписка по моему проекту (клиент)
// @Description Только клиентская ветка: переписку менеджера с креаторами и
// @Description внутренние заметки клиент не видит.
// @Tags     client-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} commentsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или чужой"
// @Router   /me/projects/{id}/comments [get]
func (h *Handler) ClientListComments(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	if _, err := h.svc.GetClientProject(r.Context(), pid, uid); err != nil {
		writeServiceErr(w, err)
		return
	}
	items, err := h.svc.ListThread(r.Context(), pid, ThreadClient, nil)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

// ClientMentionCandidates godoc
// @Summary  Кого можно упомянуть в переписке (клиент)
// @Description Список для выпадашки @. Он же — множество, по которому
// @Description упоминания проверяются на записи.
// @Tags     client-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} participantsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или чужой"
// @Router   /me/projects/{id}/comments/participants [get]
func (h *Handler) ClientMentionCandidates(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	if _, err := h.svc.GetClientProject(r.Context(), pid, uid); err != nil {
		writeServiceErr(w, err)
		return
	}
	items, err := h.svc.ThreadParticipants(r.Context(), pid, ThreadClient, nil)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, participantsResp{Items: items})
}

// ---- Creator: своя ветка ----
//
// Одни и те же три ручки обслуживают и креатора «под ключ», и исполнителя
// общего проекта: какая ветка кому принадлежит, решает ResolveCreatorThread.
// Развилка там одна — в общем проекте менеджера нет, и исполнитель говорит
// с клиентом напрямую.

// CreatorListComments godoc
// @Summary  Моя переписка по проекту (креатор/исполнитель)
// @Description Креатор «под ключ» видит только свою ветку с менеджером:
// @Description ни переписки клиента, ни веток других креаторов. В общем
// @Description проекте это переписка с клиентом.
// @Tags     creator-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} commentsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или вы не в его составе"
// @Router   /me/creator/projects/{id}/comments [get]
func (h *Handler) CreatorListComments(w http.ResponseWriter, r *http.Request) {
	uid, pid, thread, threadUser, ok := h.creatorThread(w, r)
	if !ok {
		return
	}
	_ = uid
	items, err := h.svc.ListThread(r.Context(), pid, thread, threadUser)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

// CreatorCreateComment godoc
// @Summary  Написать в переписку по проекту (креатор/исполнитель)
// @Tags     creator-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    body body commentReq true "body"
// @Success  201  {object} Comment
// @Failure  400  {object} errorResponse "bad_json; bad_id; empty_comment — пустое сообщение; invalid_input — превышена длина или неизвестный body_format"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или вы не в его составе"
// @Router   /me/creator/projects/{id}/comments [post]
func (h *Handler) CreatorCreateComment(w http.ResponseWriter, r *http.Request) {
	uid, pid, thread, threadUser, ok := h.creatorThread(w, r)
	if !ok {
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	c, err := h.svc.CreateComment(r.Context(), CommentRequest{
		ProjectID: pid, AuthorID: uid, Thread: thread, ThreadUserID: threadUser,
		Body: in.Body, Format: in.Format,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// CreatorMentionCandidates godoc
// @Summary  Кого можно упомянуть в переписке (креатор/исполнитель)
// @Tags     creator-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} participantsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или вы не в его составе"
// @Router   /me/creator/projects/{id}/comments/participants [get]
func (h *Handler) CreatorMentionCandidates(w http.ResponseWriter, r *http.Request) {
	_, pid, thread, threadUser, ok := h.creatorThread(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ThreadParticipants(r.Context(), pid, thread, threadUser)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, participantsResp{Items: items})
}

// creatorThread — общая преамбула трёх креаторских ручек: кто спрашивает,
// про какой проект и какая ветка ему принадлежит. Ответ ResolveCreatorThread
// заодно служит проверкой доступа: не в составе проекта — ErrNotFound.
func (h *Handler) creatorThread(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, string, *uuid.UUID, bool) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, "", nil, false
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, "", nil, false
	}
	thread, threadUser, err := h.svc.ResolveCreatorThread(r.Context(), pid, uid)
	if err != nil {
		writeServiceErr(w, err)
		return uuid.Nil, uuid.Nil, "", nil, false
	}
	return uid, pid, thread, threadUser, true
}

// ---- Manager: все ветки ----

// ManagerListComments godoc
// @Summary  Переписка проекта (менеджер)
// @Description Без параметров — вся переписка: клиентская ветка, ветки
// @Description креаторов и внутренние заметки. thread сужает до одной ветки;
// @Description для thread=creator нужен creator_id.
// @Tags     manager-projects
// @Produce  json
// @Security BearerAuth
// @Param    id         path  string true  "project id"
// @Param    thread     query string false "client | creator | internal"
// @Param    creator_id query string false "чья ветка, при thread=creator"
// @Success  200 {object} commentsResp
// @Failure  400 {object} errorResponse "bad_id; bad_thread — неизвестное значение; bad_creator_id — не uuid или не задан при thread=creator"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/comments [get]
func (h *Handler) ManagerListComments(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	thread := r.URL.Query().Get("thread")
	if thread == "" {
		items, err := h.svc.ListAllComments(r.Context(), pid)
		if err != nil {
			writeManagerErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
		return
	}
	threadUser, ok := queryThreadUser(w, thread, r.URL.Query().Get("creator_id"))
	if !ok {
		return
	}
	items, err := h.svc.ListThread(r.Context(), pid, thread, threadUser)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

// queryThreadUser разбирает пару thread/creator_id из query-параметров.
func queryThreadUser(w http.ResponseWriter, thread, creatorID string) (*uuid.UUID, bool) {
	switch thread {
	case ThreadClient, ThreadInternal:
		return nil, true
	case ThreadCreator:
		id, err := uuid.Parse(creatorID)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_creator_id",
				"Для ветки creator нужен creator_id — чья это переписка.")
			return nil, false
		}
		return &id, true
	default:
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_thread",
			"Ветка бывает client, creator или internal.")
		return nil, false
	}
}

// ManagerCreateComment godoc
// @Summary  Написать в переписку проекта (менеджер)
// @Description thread выбирает ветку: client (по умолчанию), creator (нужен
// @Description creator_id) или internal. Прежнее поле is_internal=true
// @Description равнозначно thread=internal.
// @Tags     manager-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    body body commentReq true "body"
// @Success  201  {object} Comment
// @Failure  400  {object} errorResponse "bad_json; bad_id; bad_creator_id; empty_comment — пустое сообщение; invalid_input — превышена длина или неизвестный body_format"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или ведёт другой менеджер; not_a_project_creator — creator_id не в составе проекта"
// @Router   /manager/projects/{id}/comments [post]
func (h *Handler) ManagerCreateComment(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	thread, threadUser, ok := h.staffThread(w, r, pid, in)
	if !ok {
		return
	}
	c, err := h.svc.CreateComment(r.Context(), CommentRequest{
		ProjectID: pid, AuthorID: uid, Thread: thread, ThreadUserID: threadUser,
		Body: in.Body, Format: in.Format,
	})
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// ManagerMentionCandidates godoc
// @Summary  Кого можно упомянуть в ветке (менеджер)
// @Tags     manager-projects
// @Produce  json
// @Security BearerAuth
// @Param    id         path  string true  "project id"
// @Param    thread     query string false "client (по умолчанию) | creator | internal"
// @Param    creator_id query string false "чья ветка, при thread=creator"
// @Success  200 {object} participantsResp
// @Failure  400 {object} errorResponse "bad_id; bad_thread; bad_creator_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/comments/participants [get]
func (h *Handler) ManagerMentionCandidates(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	thread := r.URL.Query().Get("thread")
	if thread == "" {
		thread = ThreadClient
	}
	threadUser, ok := queryThreadUser(w, thread, r.URL.Query().Get("creator_id"))
	if !ok {
		return
	}
	items, err := h.svc.ThreadParticipants(r.Context(), pid, thread, threadUser)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, participantsResp{Items: items})
}

// managerProject — общая преамбула менеджерских ручек чтения: разобрать id
// и убедиться, что проект вообще его.
func (h *Handler) managerProject(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return uuid.Nil, false
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return uuid.Nil, false
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return uuid.Nil, false
	}
	return pid, true
}

// projectIDParam — {id} из пути. Вынесен, потому что повторялся в каждой
// ручке этого файла одним и тем же куском.
func projectIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return uuid.Nil, false
	}
	return pid, true
}

// ManagerListEvents godoc
// @Summary  Лента активности проекта (менеджер)
// @Tags     manager-projects
// @Produce  json
// @Security BearerAuth
// @Param    id     path  string true  "project id"
// @Param    limit  query int    false "default 50, max 200"
// @Param    offset query int    false "default 0"
// @Success  200 {object} eventsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/events [get]
func (h *Handler) ManagerListEvents(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, err := h.svc.ListEvents(r.Context(), pid, limit, offset)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, eventsResp{Items: items})
}

// ---- Admin: те же ручки, но без assigned-фильтра ----

// AdminListProjectEvents godoc
// @Summary  Лента активности любого проекта (админ)
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} eventsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  403 {object} errorResponse "forbidden_role — нужна роль админа"
// @Router   /admin/projects/{id}/events [get]
func (h *Handler) AdminListProjectEvents(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, err := h.svc.ListEvents(r.Context(), pid, limit, offset)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, eventsResp{Items: items})
}

// AdminListProjectComments godoc
// @Summary  Переписка любого проекта (админ)
// @Description Все ветки сразу; thread сужает до одной.
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id         path  string true  "project id"
// @Param    thread     query string false "client | creator | internal"
// @Param    creator_id query string false "чья ветка, при thread=creator"
// @Success  200 {object} commentsResp
// @Failure  400 {object} errorResponse "bad_id; bad_thread; bad_creator_id"
// @Failure  403 {object} errorResponse "forbidden_role — нужна роль админа"
// @Router   /admin/projects/{id}/comments [get]
func (h *Handler) AdminListProjectComments(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	thread := r.URL.Query().Get("thread")
	if thread == "" {
		items, err := h.svc.ListAllComments(r.Context(), pid)
		if err != nil {
			writeManagerErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
		return
	}
	threadUser, ok := queryThreadUser(w, thread, r.URL.Query().Get("creator_id"))
	if !ok {
		return
	}
	items, err := h.svc.ListThread(r.Context(), pid, thread, threadUser)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

// AdminCreateProjectComment godoc
// @Summary  Написать в переписку любого проекта (админ)
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    body body commentReq true "body"
// @Success  201  {object} Comment
// @Failure  400  {object} errorResponse "bad_json; bad_id; bad_creator_id; empty_comment; invalid_input"
// @Failure  403  {object} errorResponse "forbidden_role — нужна роль админа"
// @Failure  404  {object} errorResponse "not_a_project_creator — creator_id не в составе проекта"
// @Router   /admin/projects/{id}/comments [post]
func (h *Handler) AdminCreateProjectComment(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	thread, threadUser, ok := h.staffThread(w, r, pid, in)
	if !ok {
		return
	}
	c, err := h.svc.CreateComment(r.Context(), CommentRequest{
		ProjectID: pid, AuthorID: actorID, Thread: thread, ThreadUserID: threadUser,
		Body: in.Body, Format: in.Format,
	})
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// типы для swaggo
type commentsResp struct {
	Items []Comment `json:"items"`
}

type participantsResp struct {
	Items []Participant `json:"items"`
}

type eventsResp struct {
	Items []Event `json:"items"`
}
