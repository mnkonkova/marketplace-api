package projects

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Общий проект целиком лежит на двух людях: клиент завёл, исполнитель сдал,
// клиент принял. Менеджерских ручек здесь нет и не предполагается — по
// требованиям менеджер в этом виде не участвует вовсе.

type generalCreateReq struct {
	// SpecialistID — кого выбрал клиент. Обязателен.
	SpecialistID string `json:"specialist_user_id"`
	Title        string `json:"title"`
	// Brief — что нужно сделать.
	Brief string `json:"brief"`
	// DueDate — срок, YYYY-MM-DD. Единственный срок проекта.
	DueDate string `json:"due_date"`
	Budget  *int   `json:"budget,omitempty"`
}

type generalDeliverReq struct {
	Note      string          `json:"note"`
	Materials []MaterialInput `json:"materials"`
}

type generalReasonReq struct {
	Reason string `json:"reason"`
}

type generalListResp struct {
	Items []GeneralProject `json:"items"`
}

// ClientCreateGeneral godoc
// @Summary  Завести общий проект
// @Description Клиент сам выбирает исполнителя и ставит один срок. Ни
// @Description воронки, ни выкладок, ни менеджера: задача уходит
// @Description исполнителю уведомлением, дальше сдача и приёмка.
// @Tags     client-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body generalCreateReq true "body"
// @Success  201  {object} GeneralProject
// @Failure  400  {object} errorResponse "bad_json; bad_specialist_id — не uuid; bad_due_date — не дата в формате YYYY-MM-DD; invalid_input — пустое название, срок в прошлом или дальше года, заказ у самого себя"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  422  {object} errorResponse "specialist_unavailable — профиль не опубликован, аккаунт выключен или это сотрудник"
// @Router   /me/general-projects [post]
func (h *Handler) ClientCreateGeneral(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	var in generalCreateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	specialistID, err := uuid.Parse(in.SpecialistID)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_specialist_id",
			"Не выбран исполнитель.")
		return
	}
	due, err := time.Parse("2006-01-02", in.DueDate)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_due_date",
			"Срок нужен в формате ГГГГ-ММ-ДД.")
		return
	}
	p, err := h.svc.CreateGeneral(r.Context(), CreateGeneralInput{
		ClientID: uid, SpecialistID: specialistID,
		Title: in.Title, Brief: in.Brief, DueDate: due, Budget: in.Budget,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

// ClientListGeneral godoc
// @Summary  Мои общие проекты (клиент)
// @Tags     client-projects
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} generalListResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/general-projects [get]
func (h *Handler) ClientListGeneral(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListGeneral(r.Context(), uid, true)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, generalListResp{Items: items})
}

// ClientGetGeneral godoc
// @Summary  Карточка общего проекта (клиент)
// @Description Вместе с журналом сдач: что и когда сдавали, что принято,
// @Description что вернули и почему.
// @Tags     client-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} GeneralProject
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или он не ваш"
// @Router   /me/general-projects/{id} [get]
func (h *Handler) ClientGetGeneral(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetGeneral(r.Context(), pid, uid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ClientAcceptDelivery godoc
// @Summary  Принять работу (клиент)
// @Description Принятие закрывает проект: другого конца у общего проекта нет.
// @Tags     client-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} GeneralProject
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или он не ваш"
// @Failure  409 {object} errorResponse "no_pending_delivery — исполнитель ещё ничего не сдал; project_closed — проект уже принят или отменён"
// @Router   /me/general-projects/{id}/accept [post]
func (h *Handler) ClientAcceptDelivery(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	p, err := h.svc.AcceptDelivery(r.Context(), pid, uid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ClientReworkDelivery godoc
// @Summary  Вернуть работу на доработку (клиент)
// @Tags     client-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string           true "project id"
// @Param    body body generalReasonReq true "что не так"
// @Success  200  {object} GeneralProject
// @Failure  400  {object} errorResponse "bad_json; bad_id; invalid_input — не указано, что доработать"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или он не ваш"
// @Failure  409  {object} errorResponse "no_pending_delivery — исполнитель ещё ничего не сдал; project_closed — проект уже принят или отменён; rework_limit — правки по проекту закончились"
// @Router   /me/general-projects/{id}/rework [post]
func (h *Handler) ClientReworkDelivery(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	var in generalReasonReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.ReworkDelivery(r.Context(), pid, uid, in.Reason)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ClientCancelGeneral godoc
// @Summary  Отменить общий проект (клиент)
// @Description Отменить проект со сданной и неразобранной работой нельзя —
// @Description сначала примите её или верните на доработку.
// @Tags     client-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string           true  "project id"
// @Param    body body generalReasonReq false "причина"
// @Success  204  "отменено"
// @Failure  400  {object} errorResponse "bad_json; bad_id"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или он не ваш"
// @Failure  409  {object} errorResponse "delivery_pending — работа сдана и ждёт вашего ответа; project_closed — проект уже принят или отменён"
// @Router   /me/general-projects/{id}/cancel [post]
func (h *Handler) ClientCancelGeneral(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	// Тело необязательно: причина — вежливость, а не условие отмены.
	var in generalReasonReq
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := h.svc.CancelGeneral(r.Context(), pid, uid, in.Reason); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Исполнитель ----

// SpecialistListGeneral godoc
// @Summary  Общие проекты, где я исполнитель
// @Tags     specialist-projects
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} generalListResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/specialist/general-projects [get]
func (h *Handler) SpecialistListGeneral(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListGeneral(r.Context(), uid, false)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, generalListResp{Items: items})
}

// SpecialistGetGeneral godoc
// @Summary  Карточка общего проекта (исполнитель)
// @Tags     specialist-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} GeneralProject
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или вы не его исполнитель"
// @Router   /me/specialist/general-projects/{id} [get]
func (h *Handler) SpecialistGetGeneral(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetGeneral(r.Context(), pid, uid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// SpecialistDeliver godoc
// @Summary  Сдать работу (исполнитель)
// @Description Материалы — ссылки на результат, до 20 штук. Пока клиент не
// @Description ответил, сдать второй раз нельзя.
// @Tags     specialist-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string            true "project id"
// @Param    body body generalDeliverReq true "body"
// @Success  201  {object} Delivery
// @Failure  400  {object} errorResponse "bad_json; bad_id; invalid_input — нет ни ссылки, ни комментария; больше 20 материалов; ссылка не http(s); неизвестный kind материала"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или вы не его исполнитель"
// @Failure  409  {object} errorResponse "delivery_pending — предыдущая сдача ещё не разобрана; project_closed — проект принят или отменён"
// @Router   /me/specialist/general-projects/{id}/deliver [post]
func (h *Handler) SpecialistDeliver(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, ok := projectIDParam(w, r)
	if !ok {
		return
	}
	var in generalDeliverReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	d, err := h.svc.Deliver(r.Context(), DeliverInput{
		ProjectID: pid, UserID: uid, Note: in.Note, Materials: in.Materials,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, d)
}
