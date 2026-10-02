package publications

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type listResp struct {
	Items []Publication `json:"items"`
}

type checklistResp struct {
	Items []ChecklistItem `json:"items"`
}

// ---- менеджер ----
// effectiveManagerID — для админа возвращает uuid.Nil (доступ ко всему),
// для менеджера — его собственный id. Тот же приём, что в
// internal/projects: админ ходит менеджерскими URL и не должен упираться
// в фильтр «назначен на проект».
func effectiveManagerID(r *http.Request, uid uuid.UUID) uuid.UUID {
	if role, _ := auth.RoleFrom(r.Context()); role == auth.RoleAdmin {
		return uuid.Nil
	}
	return uid
}

// managerProject — id проекта из пути + проверка, что он доступен этому
// менеджеру. Роль проверяет middleware, а вот «чей это проект» — нет:
// без второй проверки менеджер, знающий чужой project_id, получал бы
// и отчёт, и право менять состав.
func (h *Handler) managerProject(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return uuid.Nil, uuid.Nil, false
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return uuid.Nil, uuid.Nil, false
	}
	if err := h.svc.ManagerHasAccess(r.Context(), projectID, effectiveManagerID(r, uid)); err != nil {
		writeErr(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	return projectID, uid, true
}

// managerPublication — то же, когда в пути только id выкладки: сначала
// узнаём её проект, потом проверяем доступ к нему.
func (h *Handler) managerPublication(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return uuid.Nil, uuid.Nil, false
	}
	pubID, err := pathUUID(r, "pub_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id выкладки.")
		return uuid.Nil, uuid.Nil, false
	}
	projectID, err := h.svc.ProjectOfPublication(r.Context(), pubID)
	if err != nil {
		writeErr(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	if err := h.svc.ManagerHasAccess(r.Context(), projectID, effectiveManagerID(r, uid)); err != nil {
		writeErr(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	return pubID, uid, true
}

type batchReq struct {
	CreatorUserIDs []uuid.UUID `json:"creator_user_ids"`
	Scheme         Scheme      `json:"scheme"`
	From           string      `json:"from"`
	To             string      `json:"to"`
	// Dates — альтернатива схеме: произвольный список дат (YYYY-MM-DD).
	Dates         []string `json:"dates"`
	DraftLeadDays int      `json:"draft_lead_days"`
	// PerDay — сколько роликов ставить на каждый день, по умолчанию
	// один. Это цель, а не прибавка: план приводится к заданному числу,
	// и повторная отправка той же формы не добавляет ничего (см.
	// ON CONFLICT DO NOTHING в CreateBatch).
	PerDay int `json:"per_day"`
}

type previewResp struct {
	Dates []string `json:"dates"`
	Total int      `json:"total"`
}

// ManagerPreviewBatch godoc
// @Summary  Предпросмотр пачки выкладок (менеджер)
// @Description Показывает, какие даты будут созданы, до создания. Массовая
// @Description простановка — единственное место, где одна ошибка стоит ручной
// @Description чистки десятков строк.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body batchReq true "параметры пачки"
// @Success  200 {object} previewResp
// @Failure      400  {object}  errorResponse  "bad_json — не удалось разобрать тело; invalid_input — дата не в формате ГГГГ-ММ-ДД; bad_schedule — неизвестная схема или слишком большой диапазон"
// @Failure      401  {object}  errorResponse  "no_user — сессия истекла"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или назначен другому менеджеру"
// @Router   /manager/projects/{id}/publications/preview [post]
func (h *Handler) ManagerPreviewBatch(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.managerProject(w, r); !ok {
		return
	}
	var req batchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	from, to, err := parseRange(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	dates, total, err := h.svc.PreviewBatch(req.Scheme, req.CreatorUserIDs, from, to, req.PerDay)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]string, len(dates))
	for i, d := range dates {
		out[i] = d.Format(dateLayout)
	}
	httpx.WriteJSON(w, http.StatusOK, previewResp{Dates: out, Total: total})
}

// ManagerCreateBatch godoc
// @Summary  Проставить даты выкладок пачкой (менеджер)
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body batchReq true "параметры пачки"
// @Success  201 {object} BatchResult
// @Failure      400  {object}  errorResponse  "bad_json; bad_date — дата не в формате ГГГГ-ММ-ДД; bad_schedule; invalid_input — не выбраны креаторы или даты, пачка больше 500; nothing_to_create"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Failure      409  {object}  errorResponse  "wrong_project_kind — выкладки бывают только у «креаторы под ключ»; creator_not_in_project — креатора нет в составе"
// @Router   /manager/projects/{id}/publications/batch [post]
func (h *Handler) ManagerCreateBatch(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req batchReq
	var err error
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}

	var res BatchResult
	if len(req.Dates) > 0 {
		dates := make([]time.Time, 0, len(req.Dates))
		for _, s := range req.Dates {
			d, perr := time.Parse(dateLayout, s)
			if perr != nil {
				httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date", "Дата должна быть в формате ГГГГ-ММ-ДД: "+s)
				return
			}
			dates = append(dates, d)
		}
		res, err = h.svc.CreateBatch(r.Context(), CreateBatchInput{
			ProjectID:      projectID,
			CreatorUserIDs: req.CreatorUserIDs,
			Dates:          dates,
			DraftLeadDays:  req.DraftLeadDays,
			PerDay:         req.PerDay,
			CreatedBy:      uid,
		})
	} else {
		from, to, rerr := parseRange(req)
		if rerr != nil {
			writeErr(w, rerr)
			return
		}
		res, err = h.svc.CreateBatchByScheme(r.Context(), projectID, req.CreatorUserIDs,
			req.Scheme, from, to, req.DraftLeadDays, req.PerDay, uid)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

type cancelBatchReq struct {
	BatchID uuid.UUID `json:"batch_id"`
}

type cancelBatchResp struct {
	Cancelled int `json:"cancelled"`
}

// ManagerCancelBatch godoc
// @Summary  Отменить пачку выкладок (менеджер)
// @Description Отменяются только те выкладки, по которым ещё ничего не сдано.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body cancelBatchReq true "id пачки"
// @Success  200 {object} cancelBatchResp
// @Failure      400  {object}  errorResponse  "bad_json — нужен batch_id отменяемой пачки; bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Router   /manager/projects/{id}/publications/cancel_batch [post]
func (h *Handler) ManagerCancelBatch(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req cancelBatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BatchID == uuid.Nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Нужен batch_id отменяемой пачки.")
		return
	}
	n, err := h.svc.CancelBatch(r.Context(), projectID, req.BatchID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, cancelBatchResp{Cancelled: n})
}

// ManagerList godoc
// @Summary  Все выкладки проекта (менеджер)
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} listResp
// @Failure      400  {object}  errorResponse  "bad_id — неверный id проекта"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Router   /manager/projects/{id}/publications [get]
func (h *Handler) ManagerList(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListForManager(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listResp{Items: items})
}

type closeReq struct {
	Reason string `json:"reason"`
}

type editLinkReq struct {
	// URL — новый адрес ролика. Пусто — снять ссылку с площадки.
	URL string `json:"url"`
}

type reviewReq struct {
	// Marks — вердикты по пунктам целиком: что не прислали, того
	// менеджер не отмечал.
	Marks []ReviewMark `json:"marks"`
	// Comment — замечание. Обязательно при decision=return.
	Comment string `json:"comment"`
	// Decision — пусто «сохранить ход проверки», return «вернуть»,
	// accept «принять».
	Decision string `json:"decision"`
}

// ManagerReview godoc
// @Summary  Проверка ролика: вердикты по чек-листу и решение (менеджер)
// @Description Менеджер отмечает по каждому пункту «да» или «нет» и выносит решение: вернуть с замечанием или принять. Принять нельзя, пока хоть один обязательный пункт сданных площадок не отмечен «да» — непроверенное считается непройденным. Пустое decision сохраняет ход проверки, не вынося решения: ролик смотрят частями. Новая сдача ссылок отменяет прежнее решение и стирает вердикты — они относились к другому ролику.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    body body reviewReq true "вердикты и решение"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input — возврат без замечания или неизвестное решение; foreign_checklist_item"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка в чужом проекте"
// @Failure      409  {object}  errorResponse  "publication_closed; nothing_to_review — ссылок нет"
// @Failure      422  {object}  errorResponse  "review_blocked — обязательные пункты не пройдены"
// @Router   /manager/publications/{pub_id}/review [post]
func (h *Handler) ManagerReview(w http.ResponseWriter, r *http.Request) {
	pubID, uid, ok := h.managerPublication(w, r)
	if !ok {
		return
	}
	projectID, err := h.svc.ProjectOfPublication(r.Context(), pubID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !h.requireFeature(w, r, projectID, hasReview, whyNoReview) {
		return
	}
	var req reviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.Review(r.Context(), ReviewInput{
		PublicationID: pubID,
		ManagerUserID: uid,
		Marks:         req.Marks,
		Comment:       req.Comment,
		Decision:      req.Decision,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// ManagerEditLink godoc
// @Summary  Исправить сданную ссылку (менеджер)
// @Description Ссылку сдаёт креатор, и ошибается в ней тоже он. Менеджер правит адрес на месте; пустой url снимает ссылку с площадки, и выкладка снова становится неполной. Если ролик другой — ежедневные замеры этой ссылки удаляются, иначе история двух разных видео склеилась бы в одну линию.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    platform path string true "площадка: tiktok|instagram|youtube|vk|likee"
// @Param    body body editLinkReq true "новый адрес"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input — неизвестная площадка, нераспознанная ссылка или ссылка другой площадки"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка в чужом проекте либо ссылки на этой площадке нет"
// @Failure      409  {object}  errorResponse  "publication_closed — выкладка отменена или закрыта руками"
// @Router   /manager/publications/{pub_id}/links/{platform} [put]
func (h *Handler) ManagerEditLink(w http.ResponseWriter, r *http.Request) {
	pubID, uid, ok := h.managerPublication(w, r)
	if !ok {
		return
	}
	var req editLinkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.ManagerEditLink(r.Context(), ManagerEditLinkInput{
		PublicationID: pubID,
		ManagerUserID: uid,
		Platform:      chi.URLParam(r, "platform"),
		URL:           req.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// CreatorEditLink godoc
// @Summary  Переслать свою ссылку (креатор)
// @Description Ролик удаляют с площадки, аккаунт перевыкладывают, короткая
// @Description ссылка протухает — и новый адрес есть ровно у автора. Раньше
// @Description он писал его в переписку, а переносил менеджер.
// @Description Снять площадку креатор не может: пустой адрес означает «ролика
// @Description не было», и это решение о работе, а не о ссылке.
// @Description Если ролик другой — ежедневные замеры этой ссылки удаляются, а
// @Description проверка открывается заново: проверяли не его.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    platform path string true "площадка: tiktok|instagram|youtube|vk|likee"
// @Param    body body editLinkReq true "новый адрес"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input; link_remove_denied — пустой адрес"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или заведена на другого креатора"
// @Failure      409  {object}  errorResponse  "publication_closed; period_locked — период уже подытожен"
// @Router   /me/creator/publications/{pub_id}/links/{platform} [put]
func (h *Handler) CreatorEditLink(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	pubID, err := pathUUID(r, "pub_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id выкладки.")
		return
	}
	var req editLinkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.CreatorEditLink(r.Context(), ManagerEditLinkInput{
		PublicationID: pubID,
		ManagerUserID: uid,
		Platform:      chi.URLParam(r, "platform"),
		URL:           req.URL,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// ManagerClosePublication godoc
// @Summary  Закрыть неполную выкладку вручную (менеджер)
// @Description Исключение из правила «закрыто на пяти ссылках». Причина обязательна.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    body body closeReq true "причина закрытия"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input — закрытие неполной выкладки требует причины"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или в чужом проекте"
// @Failure      409  {object}  errorResponse  "publication_closed — выкладка уже закрыта или отменена"
// @Router   /manager/publications/{pub_id}/close [post]
func (h *Handler) ManagerClosePublication(w http.ResponseWriter, r *http.Request) {
	pubID, uid, ok := h.managerPublication(w, r)
	if !ok {
		return
	}
	var req closeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.CloseManually(r.Context(), CloseManuallyInput{
		PublicationID: pubID, ManagerUserID: uid, Reason: req.Reason,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

type addPubReq struct {
	// CreatorUserID — кому поручена выкладка.
	//
	// Строкой, а не uuid.UUID: у проекта без креаторов владельца нет,
	// и поле приходит пустым. uuid.UUID пустую строку разобрать не
	// умеет — запрос падал на декодере с «bad_json» ещё до того, как
	// кто-нибудь посмотрел на вид проекта.
	CreatorUserID string `json:"creator_user_id"`
	DueDate       string `json:"due_date"`
	DraftLeadDays int    `json:"draft_lead_days"`
}

// ManagerAddPublication godoc
// @Summary  Поставить одну выкладку на дату (менеджер)
// @Description Правка плана по одной строке: пачкой ставят месяц вперёд, а дальше состав и даты меняются поштучно.
// @Description У проекта без креаторов creator_user_id не передаётся: выкладка принадлежит проекту.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body addPubReq true "кому и на когда"
// @Success  201 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_date — дата не в формате ГГГГ-ММ-ДД; invalid_input — дата в прошлом или дальше чем на год"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Failure      409  {object}  errorResponse  "wrong_project_kind; creator_not_in_project — креатора нет в составе (в том числе когда его не назвали вовсе); wrong_project_kind — креатор назван у проекта без состава; day_taken — на этот день уже есть выкладка; period_locked"
// @Router   /manager/projects/{id}/publications [post]
func (h *Handler) ManagerAddPublication(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req addPubReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Нужны creator_user_id и due_date.")
		return
	}
	// Пустой creator_user_id здесь НЕ ошибка формата: у проекта без
	// креаторов выкладка принадлежит проекту, и владельца у неё нет
	// вовсе. Кто прав, а кто нет, решает репозиторий — он один читает
	// вид проекта: там «пусто» у проекта с составом станет
	// creator_not_in_project, а названный человек у проекта без
	// состава — wrong_project_kind. Отказывать раньше него значило бы
	// закрыть простановку дат тому виду проекта, ради которого её и
	// научили обходиться без людей.
	var creator uuid.UUID
	if s := strings.TrimSpace(req.CreatorUserID); s != "" {
		parsed, perr := uuid.Parse(s)
		if perr != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "creator_user_id не похож на идентификатор.")
			return
		}
		creator = parsed
	}
	day, err := time.Parse(dateLayout, req.DueDate)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date", "Дата должна быть в формате ГГГГ-ММ-ДД.")
		return
	}
	got, err := h.svc.ManagerAddPublication(r.Context(), AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: creator,
		Day:           day,
		DraftLeadDays: req.DraftLeadDays,
		ManagerUserID: uid,
		Now:           time.Now(),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, got)
}

type dueDateReq struct {
	DueDate string `json:"due_date"`
}

// ManagerMoveDueDate godoc
// @Summary  Перенести дату выкладки (менеджер)
// @Description Только по плановой выкладке, по которой ещё не сдавали ссылки. Открытая просьба креатора о переносе закрывается этим же действием.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    body body dueDateReq true "новая дата"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_date; bad_id; invalid_input — дата в прошлом или дальше чем на год"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или в чужом проекте"
// @Failure      409  {object}  errorResponse  "publication_started — по выкладке уже сдавали ссылки; day_taken; period_locked"
// @Router   /manager/publications/{pub_id}/due_date [put]
func (h *Handler) ManagerMoveDueDate(w http.ResponseWriter, r *http.Request) {
	pubID, uid, ok := h.managerPublication(w, r)
	if !ok {
		return
	}
	var req dueDateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	day, err := time.Parse(dateLayout, req.DueDate)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date", "Дата должна быть в формате ГГГГ-ММ-ДД.")
		return
	}
	got, err := h.svc.MoveDueDate(r.Context(), MoveDueDateInput{
		PublicationID: pubID, ManagerUserID: uid, Day: day, Now: time.Now(),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// ManagerCancelPublication godoc
// @Summary  Снять запланированную выкладку (менеджер)
// @Description Не то же, что «закрыть неполную»: там ролик вышел не везде, здесь выкладки не будет вовсе. Сданное не снимается.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    body body closeReq true "причина"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input — причина длиннее 300 символов"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или в чужом проекте"
// @Failure      409  {object}  errorResponse  "publication_started — по выкладке уже сдавали ссылки"
// @Router   /manager/publications/{pub_id}/cancel [post]
func (h *Handler) ManagerCancelPublication(w http.ResponseWriter, r *http.Request) {
	pubID, uid, ok := h.managerPublication(w, r)
	if !ok {
		return
	}
	var req closeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.CancelPublication(r.Context(), pubID, uid, req.Reason)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

type creatorReq struct {
	CreatorUserID uuid.UUID `json:"creator_user_id"`
}

// ManagerAddCreator godoc
// @Summary  Добавить креатора в проект (менеджер)
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body creatorReq true "кого добавляем"
// @Success  204
// @Failure      400  {object}  errorResponse  "bad_json — нужен creator_user_id; bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект или пользователь не найден"
// @Failure      409  {object}  errorResponse  "not_a_creator — не специалист либо аккаунт отключён"
// @Router   /manager/projects/{id}/creators [post]
func (h *Handler) ManagerAddCreator(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, projectID, hasCrew, whyNoCrew) {
		return
	}
	var req creatorReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CreatorUserID == uuid.Nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Нужен creator_user_id.")
		return
	}
	if err := h.svc.AddCreator(r.Context(), projectID, req.CreatorUserID, uid); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ManagerRemoveCreator godoc
// @Summary  Убрать креатора из проекта (менеджер)
// @Description Мягкое удаление: сданные выкладки и цифры в отчёте остаются.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    creator_id path string true "creator user id"
// @Success  204
// @Failure      400  {object}  errorResponse  "bad_id — неверный id проекта или креатора"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Failure      409  {object}  errorResponse  "creator_not_in_project — этого креатора нет в составе"
// @Router   /manager/projects/{id}/creators/{creator_id} [delete]
func (h *Handler) ManagerRemoveCreator(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, projectID, hasCrew, whyNoCrew) {
		return
	}
	creatorID, err := pathUUID(r, "creator_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id креатора.")
		return
	}
	if err := h.svc.RemoveCreator(r.Context(), projectID, creatorID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type snapshotReq struct {
	TemplateID uuid.UUID `json:"template_id"`
}

type snapshotResp struct {
	Copied int `json:"copied"`
}

// ManagerSnapshotChecklist godoc
// @Summary  Скопировать чеклист из библиотеки в проект (менеджер)
// @Description Снимок: последующая правка библиотеки не меняет этот проект.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body snapshotReq true "шаблон чек-листа"
// @Success  200 {object} snapshotResp
// @Failure      400  {object}  errorResponse  "bad_json — нужен template_id; bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Router   /manager/projects/{id}/checklist [post]
func (h *Handler) ManagerSnapshotChecklist(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, projectID, hasChecklist, whyNoChecklist) {
		return
	}
	var req snapshotReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TemplateID == uuid.Nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Нужен template_id шаблона чеклиста.")
		return
	}
	n, err := h.svc.SnapshotChecklist(r.Context(), projectID, req.TemplateID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, snapshotResp{Copied: n})
}

// ---- пункт чек-листа под конкретный проект ----

type checklistItemReq struct {
	Text string `json:"text"`
	// Platform пустой = пункт общий для всех площадок.
	Platform   string `json:"platform"`
	IsRequired bool   `json:"is_required"`
}

// ManagerAddChecklistItem godoc
// @Summary  Добавить пункт в чек-лист проекта (менеджер)
// @Description Пункт живёт только в этом проекте: в библиотеку не попадает.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body checklistItemReq true "пункт"
// @Success  201 {object} ChecklistItem
// @Failure      400  {object}  errorResponse  "bad_json; invalid_input — пустой или слишком длинный текст"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Router   /manager/projects/{id}/checklist/items [post]
func (h *Handler) ManagerAddChecklistItem(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, projectID, hasChecklist, whyNoChecklist) {
		return
	}
	var req checklistItemReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	it, err := h.svc.AddChecklistItem(r.Context(), projectID, req.Text, req.Platform, req.IsRequired)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, it)
}

type checklistRequiredReq struct {
	IsRequired *bool `json:"is_required"`
}

// ManagerSetChecklistItemRequired godoc
// @Summary  Сделать пункт чек-листа обязательным или необязательным (менеджер)
// @Description Чек-лист прицепился к проекту шаблоном, и менеджер его ПРАВИТ,
// @Description а не собирает заново: «обложка вертикальная» в одном проекте
// @Description обязательна, а в другом — пожелание. Отметки проверки при этом
// @Description сохраняются — в отличие от «удалить и завести заново».
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    itemId path string true "checklist item id"
// @Param    body body checklistRequiredReq true "обязательность"
// @Success  200 {object} ChecklistItem
// @Failure      400  {object}  errorResponse  "bad_id; bad_json; invalid_input — не указано, обязателен пункт или нет"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект или пункт не найден"
// @Router   /manager/projects/{id}/checklist/items/{itemId} [patch]
func (h *Handler) ManagerSetChecklistItemRequired(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, projectID, hasChecklist, whyNoChecklist) {
		return
	}
	itemID, err := pathUUID(r, "itemId")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id пункта.")
		return
	}
	var req checklistRequiredReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	if req.IsRequired == nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input",
			"Укажите, обязателен пункт или нет.")
		return
	}
	it, err := h.svc.SetChecklistItemRequired(r.Context(), projectID, itemID, *req.IsRequired)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, it)
}

// ManagerDeleteChecklistItem godoc
// @Summary  Убрать пункт из чек-листа проекта (менеджер)
// @Description Пункт, по которому уже отчитывались, не удаляется: вместе с ним исчез бы след проверки.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    itemId path string true "checklist item id"
// @Success  204
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект или пункт не найден"
// @Failure      409  {object}  errorResponse  "item_used — по пункту уже отчитывались"
// @Router   /manager/projects/{id}/checklist/items/{itemId} [delete]
func (h *Handler) ManagerDeleteChecklistItem(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if !h.requireFeature(w, r, projectID, hasChecklist, whyNoChecklist) {
		return
	}
	itemID, err := pathUUID(r, "itemId")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id пункта.")
		return
	}
	switch err := h.svc.DeleteChecklistItem(r.Context(), projectID, itemID); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrChecklistItemUsed):
		// 409, а не 400: запрос правильный, состояние мира — нет.
		httpx.WriteErrMsg(w, http.StatusConflict, "item_used",
			"По этому пункту уже отчитывались — удалить его нельзя, иначе исчезнет след проверки.")
	default:
		writeErr(w, err)
	}
}

type decideReq struct {
	Approve bool `json:"approve"`
}

// ManagerDecideDateRequest godoc
// @Summary  Решение по просьбе о переносе даты (менеджер)
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    req_id path string true "date request id"
// @Param    body body decideReq true "решение по переносу"
// @Success  204
// @Failure      400  {object}  errorResponse  "bad_json; bad_id — неверный id просьбы"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — просьба не найдена, уже рассмотрена или в чужом проекте"
// @Router   /manager/publication_date_requests/{req_id}/decide [post]
func (h *Handler) ManagerDecideDateRequest(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	reqID, err := pathUUID(r, "req_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id просьбы.")
		return
	}
	// В пути только id просьбы — доступ проверяем через её проект.
	projectID, err := h.svc.ProjectOfDateRequest(r.Context(), reqID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.ManagerHasAccess(r.Context(), projectID, effectiveManagerID(r, uid)); err != nil {
		writeErr(w, err)
		return
	}
	var body decideReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	if err := h.svc.DecideDateRequest(r.Context(), reqID, uid, body.Approve); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type remindResp struct {
	Sent bool `json:"sent"`
}

// ManagerRemindNow godoc
// @Summary  Напомнить о выкладке сейчас (менеджер)
// @Description Не дожидаясь утренней рассылки. Второе нажатие в тот же день
// @Description ничего не отправит: креатор не должен получать два одинаковых
// @Description сообщения подряд.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Success  200 {object} remindResp
// @Failure      400  {object}  errorResponse  "bad_id — неверный id выкладки"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или в чужом проекте"
// @Failure      409  {object}  errorResponse  "already_reminded — сегодня по этой выкладке напоминание уже отправляли"
// @Router   /manager/publications/{pub_id}/remind [post]
func (h *Handler) ManagerRemindNow(w http.ResponseWriter, r *http.Request) {
	pubID, _, ok := h.managerPublication(w, r)
	if !ok {
		return
	}
	sent, err := h.svc.RemindNow(r.Context(), pubID, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	if !sent {
		httpx.WriteErrMsg(w, http.StatusConflict, "already_reminded",
			"Сегодня по этой выкладке напоминание уже отправляли.")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, remindResp{Sent: true})
}

// ---- креатор ----

// CreatorList godoc
// @Summary  Мои выкладки в проекте (креатор)
// @Description Только свои: чужие выкладки не попадают в выдачу, а не прячутся
// @Description на фронте.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} listResp
// @Failure      400  {object}  errorResponse  "bad_id — неверный id проекта"
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/creator/projects/{id}/publications [get]
func (h *Handler) CreatorList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	items, err := h.svc.ListForCreator(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listResp{Items: items})
}

// CreatorChecklist godoc
// @Summary  Чеклист проекта (креатор)
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} checklistResp
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — вы не участник этого проекта"
// @Router   /me/creator/projects/{id}/checklist [get]
func (h *Handler) CreatorChecklist(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	// Без этой проверки чеклист проекта отдавался любому залогиненному:
	// группа в роутере не требует роли, а выдача не фильтровала ничего.
	member, err := h.svc.CreatorInProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !member {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return
	}
	items, err := h.svc.ProjectChecklist(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, checklistResp{Items: items})
}

// addSelfReq — креатор заводит себе выкладку.
type addSelfReq struct {
	// DueDate — день выхода ролика, YYYY-MM-DD. Сегодня или вперёд.
	DueDate string `json:"due_date" example:"2026-09-20"`
}

// CreatorAddPublication godoc
// @Summary  Добавить себе выкладку (креатор)
// @Description План периода выполнен, а до ступени просмотров не хватает —
// @Description креатор заводит себе ролик сам, без согласования с менеджером.
// @Description Дальше выкладка живёт как обычная: пять площадок, чеклист проекта,
// @Description сбор статистики. В знаменатель недосдачи она не попадает.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body addSelfReq true "дата выхода"
// @Success  201 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; bad_date — дата не в формате YYYY-MM-DD; invalid_input — дата в прошлом"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или вы не в его составе"
// @Failure      409  {object}  errorResponse  "day_taken — на эту дату у вас уже есть выкладка; period_locked — период подытожен; wrong_project_kind"
// @Router   /me/creator/projects/{id}/publications [post]
func (h *Handler) CreatorAddPublication(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var req addSelfReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	day, err := time.Parse("2006-01-02", strings.TrimSpace(req.DueDate))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date", "Дата в формате ГГГГ-ММ-ДД.")
		return
	}
	p, err := h.svc.AddSelfPublication(r.Context(), projectID, uid, day, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

type submitReq struct {
	URLs []string `json:"urls"`
	// Title — название ролика. Досылая площадки, поле можно не повторять:
	// пустое не затирает записанное.
	Title          string      `json:"title,omitempty"`
	CheckedItemIDs []uuid.UUID `json:"checked_item_ids"`
}

// CreatorSubmitLinks godoc
// @Summary  Сдать ролик ссылками (креатор)
// @Description Ссылки принимаются в любом виде — площадка распознаётся сама.
// @Description Пришли все пять — выкладка закрыта, одна-четыре — сдана частично.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    body body submitReq true "ссылки на выкладку"
// @Success  200 {object} Publication
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; no_links — не передано ни одной ссылки; duplicate_platform — две ссылки на одну площадку; unknown_platform — ссылка не с одной из пяти площадок"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или заведена на другого креатора"
// @Failure      409  {object}  errorResponse  "publication_closed — досылать ссылки нельзя"
// @Failure      422  {object}  errorResponse  "checklist_incomplete — не отмечены обязательные пункты, в message перечислены какие"
// @Router   /me/creator/publications/{pub_id}/links [post]
func (h *Handler) CreatorSubmitLinks(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	pubID, err := pathUUID(r, "pub_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id выкладки.")
		return
	}
	var req submitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.SubmitLinks(r.Context(), SubmitLinksInput{
		PublicationID:  pubID,
		ActorUserID:    uid,
		URLs:           req.URLs,
		Title:          req.Title,
		CheckedItemIDs: req.CheckedItemIDs,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// CreatorResubmit godoc
// @Summary  Отправить возвращённый ролик на проверку заново (креатор)
// @Description Ссылки и статистика остаются как есть: ролик тот же, изменилось
// @Description то, что в нём исправили. Нужна там, где правка идёт НА ПЛОЩАДКЕ
// @Description и адрес ролика не меняется — добавить «новую ссылку» в таком
// @Description случае нечего, а сказать «готово» надо.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Success  200 {object} Publication
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или заведена на другого креатора"
// @Failure      409  {object}  errorResponse  "nothing_to_resubmit — ролик уже на проверке; publication_closed; nothing_to_review — ссылок нет"
// @Router   /me/creator/publications/{pub_id}/resubmit [post]
func (h *Handler) CreatorResubmit(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	pubID, err := pathUUID(r, "pub_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id выкладки.")
		return
	}
	got, err := h.svc.CreatorResubmit(r.Context(), pubID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

type dateRequestReq struct {
	RequestedDate string `json:"requested_date"`
	Reason        string `json:"reason"`
}

// CreatorRequestDateChange godoc
// @Summary  Попросить перенос дедлайна (креатор)
// @Description Дедлайн ставит менеджер; креатор может только попросить.
// @Description Пока просьба не рассмотрена, выкладка не считается просроченной.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    pub_id path string true "publication id"
// @Param    body body dateRequestReq true "новая дата и причина"
// @Success  201 {object} DateRequest
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; bad_date — дата не в формате ГГГГ-ММ-ДД; invalid_input — нужна причина, перенос в прошлое"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — выкладка не найдена или чужая"
// @Failure      409  {object}  errorResponse  "already_requested — непринятая просьба уже висит"
// @Router   /me/creator/publications/{pub_id}/date_request [post]
func (h *Handler) CreatorRequestDateChange(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	pubID, err := pathUUID(r, "pub_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id выкладки.")
		return
	}
	var req dateRequestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	d, perr := time.Parse(dateLayout, req.RequestedDate)
	if perr != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date", "Дата должна быть в формате ГГГГ-ММ-ДД.")
		return
	}
	got, err := h.svc.RequestDateChange(r.Context(), pubID, uid, d, req.Reason)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, got)
}

// ---- общее ----

const dateLayout = "2006-01-02"

func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

func parseRange(req batchReq) (from, to time.Time, err error) {
	from, err = time.Parse(dateLayout, req.From)
	if err != nil {
		return time.Time{}, time.Time{}, errInvalid("Начало диапазона должно быть в формате ГГГГ-ММ-ДД.")
	}
	to, err = time.Parse(dateLayout, req.To)
	if err != nil {
		return time.Time{}, time.Time{}, errInvalid("Конец диапазона должен быть в формате ГГГГ-ММ-ДД.")
	}
	return from, to, nil
}

func errInvalid(msg string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, msg)
}

func writeNoUser(w http.ResponseWriter) {
	httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
}

// writeErr — единая карта ошибок домена в HTTP. Отдельная функция, а не
// switch в каждой ручке: иначе одна забытая ветка отдаёт 500 там, где
// пользователю нужно понятное объяснение.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNoLinks):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "no_links", "Не передано ни одной ссылки.")
	case errors.Is(err, ErrDuplicatePlatform):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "duplicate_platform",
			"Две ссылки на одну площадку — оставьте одну.")
	case errors.Is(err, ErrUnknownPlatform), errors.Is(err, ErrNotAURL):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "unknown_platform",
			"Ссылка не с одной из пяти площадок: TikTok, Instagram, YouTube, VK, Likee.")
	case errors.Is(err, ErrUnknownScheme), errors.Is(err, ErrRangeTooLong):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_schedule", err.Error())
	case errors.Is(err, ErrTemplateArchived):
		httpx.WriteErrMsg(w, http.StatusConflict, "template_archived",
			"Шаблон в архиве — сначала верните его.")
	case errors.Is(err, ErrNothingToResubmit):
		httpx.WriteErrMsg(w, http.StatusConflict, "nothing_to_resubmit",
			"Ролик уже на проверке у менеджера — отправлять его заново не нужно.")
	case errors.Is(err, ErrChecklistIncomplete):
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "checklist_incomplete",
			"Отметьте обязательные пункты чеклиста: "+httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrReviewBlocked):
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "review_blocked",
			"Принять нельзя, пока не пройдены обязательные пункты: "+httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNothingToReview):
		httpx.WriteErrMsg(w, http.StatusConflict, "nothing_to_review",
			"По этой выкладке ещё нет ни одной ссылки — смотреть нечего.")
	case errors.Is(err, ErrForeignChecklistItem):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "foreign_checklist_item",
			"Пункт не из чек-листа этого проекта.")
	case errors.Is(err, ErrCollapsedNoDetail):
		httpx.WriteErrMsg(w, http.StatusGone, "collapsed_no_detail",
			"Проект закрыт, подробная статистика по роликам больше не хранится — остались только итоги проекта.")
	case errors.Is(err, ErrSuggestionDecided):
		httpx.WriteErrMsg(w, http.StatusConflict, "suggestion_decided",
			"По этой находке уже ответили — обновите страницу.")
	case errors.Is(err, ErrSuggestionPlatformMismatch):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "platform_mismatch",
			"Ролик с другой площадки — в этот слот он не встанет.")
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Выкладка или проект не найдены.")
	case errors.Is(err, ErrForbidden):
		// Не 403: сообщать «есть такая выкладка, но не ваша» — значит
		// подтверждать её существование чужому человеку.
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Выкладка не найдена.")
	case errors.Is(err, ErrNotCreatorsProject):
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_project_kind",
			"Выкладки бывают только у проектов «креаторы под ключ» и «бренд под ключ».")
	case errors.Is(err, ErrCrewNotAllowed):
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_project_kind",
			"У проекта «бренд под ключ» креаторов не бывает: ролики выходят с аккаунтов бренда.")
	case errors.Is(err, ErrNoCreator):
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_project_kind",
			"Это ролик проекта, а не чей-то: напоминать, проверять и сдавать по нему некому.")
	case errors.Is(err, ErrNotACreator):
		httpx.WriteErrMsg(w, http.StatusConflict, "not_a_creator",
			"Этого пользователя нельзя добавить креатором: он не специалист либо аккаунт отключён.")
	case errors.Is(err, ErrCreatorNotInProject):
		httpx.WriteErrMsg(w, http.StatusConflict, "creator_not_in_project",
			"Этого креатора нет в составе проекта.")
	case errors.Is(err, ErrDayTaken):
		httpx.WriteErrMsg(w, http.StatusConflict, "day_taken",
			"Эту дату только что занял кто-то другой — нажмите ещё раз.")
	case errors.Is(err, ErrDayFull):
		httpx.WriteErrMsg(w, http.StatusConflict, "day_full",
			"На этот день уже стоит максимум роликов — выберите другой день.")
	case errors.Is(err, ErrLinkRemoveDenied):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "link_remove_denied",
			"Снять площадку может только менеджер — пришлите новый адрес или напишите ему.")
	case errors.Is(err, ErrPeriodLocked):
		httpx.WriteErrMsg(w, http.StatusConflict, "period_locked",
			"Этот период уже подытожен — добавить в него ролик нельзя.")
	case errors.Is(err, ErrPublicationStarted):
		httpx.WriteErrMsg(w, http.StatusConflict, "publication_started",
			"По этой выкладке уже сдавали ссылки — дату у неё не двигают и саму её не снимают. "+
				"Если ролик вышел не везде, закройте её с причиной.")
	case errors.Is(err, ErrPublicationClosed):
		httpx.WriteErrMsg(w, http.StatusConflict, "publication_closed",
			"Выкладка закрыта — досылать ссылки нельзя.")
	case errors.Is(err, ErrAlreadyRequested):
		httpx.WriteErrMsg(w, http.StatusConflict, "already_requested",
			"По этой выкладке уже висит нерассмотренная просьба о переносе.")
	case errors.Is(err, ErrTooManyMaterials):
		httpx.WriteErrMsg(w, http.StatusConflict, "too_many_materials",
			"В проекте уже сто материалов — удалите лишние, прежде чем добавлять.")
	case errors.Is(err, ErrNothingToCreate):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "nothing_to_create",
			"Не выбраны креаторы или даты.")
	default:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// ---- отчёт ----

// ManagerReport godoc
// @Summary  Отчёт по проекту (менеджер)
// @Description Итоги, накопительный график, сравнение креаторов и площадок,
// @Description таблица роликов. Заменяет ручную таблицу.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} Report
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Router   /manager/projects/{id}/report [get]
func (h *Handler) ManagerReport(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	rep, err := h.svc.Report(r.Context(), projectID, ReportFilter{})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rep)
}

type refreshStatsResp struct {
	// Refreshed — сколько ссылок обошли прямо сейчас. Ноль — цифры и так
	// свежие по расписанию, и это нормальный ответ, а не отказ.
	Refreshed int `json:"refreshed"`
	// Saved — по скольким пришли цифры. Меньше Refreshed — часть
	// площадок сборщик не умеет либо ролик ещё не проиндексирован.
	Saved int `json:"saved"`
}

// refreshLimit — сколько ссылок обходим за одно открытие карточки.
//
// Двадцать пять: сборщик держит два параллельных слота и тратит на
// ссылку секунды, так что больше этого человек всё равно не дождётся, а
// кредиты уйдут. Непопавшие в лимит обновит фоновый обход.
const refreshLimit = 25

// refreshStats — общий путь обновления для менеджера и креатора.
//
// Отвечает СИНХРОННО: карточка показывает загрузку на роликах и ждёт.
// Фоновая задача с опросом была бы честнее по времени ответа, но она же
// означала бы «цифры приедут когда-нибудь» — а человек открыл карточку
// именно затем, чтобы посмотреть на них сейчас.
func (h *Handler) refreshStats(
	w http.ResponseWriter, r *http.Request, projectID uuid.UUID, creatorID *uuid.UUID,
) {
	st, err := h.svc.RefreshProject(r.Context(), projectID, creatorID, time.Now(), refreshLimit)
	if errors.Is(err, ErrCollectorNotSet) {
		// Не пятисотка: сбор может быть не настроен, и это состояние
		// стенда, а не поломка. Но и не тихий успех — иначе «сбор
		// выключен» и «просмотров нет» выглядят одинаково.
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "collector_not_set",
			"Сбор статистики не настроен — обновить цифры нечем.")
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, refreshStatsResp{Refreshed: st.Considered, Saved: st.Saved})
}

// ManagerRefreshStats godoc
// @Summary  Обновить просмотры по проекту (менеджер)
// @Description Обходит ссылки проекта, которым пора обновиться, и ждёт ответа
// @Description сборщика. Что считается «пора» — затухающее расписание: минута,
// @Description пять, десять, полчаса в первые полтора часа, дальше раз в час
// @Description до конца вторых суток и раз в шесть часов на всём остальном.
// @Description Свежее своего шага не трогаем, поэтому перезагрузка страницы
// @Description кредитов у поставщика не стоит.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} refreshStatsResp
// @Failure      401  {object}  errorResponse  "no_user — сессия истекла"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или назначен другому менеджеру"
// @Failure      503  {object}  errorResponse  "collector_not_set — сбор статистики не настроен"
// @Router   /manager/projects/{id}/report/refresh [post]
func (h *Handler) ManagerRefreshStats(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	// Менеджер обновляет проект целиком: он за него и отвечает.
	h.refreshStats(w, r, projectID, nil)
}

// CreatorRefreshStats godoc
// @Summary  Обновить просмотры по проекту (креатор)
// @Description То же, что у менеджера: креатор смотрит на свои цифры теми же
// @Description глазами, и ноль у вышедшего ролика объясняется ему так же плохо.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} refreshStatsResp
// @Failure      401  {object}  errorResponse  "no_user — сессия истекла"
// @Failure      404  {object}  errorResponse  "not_found — вы не участник этого проекта"
// @Failure      503  {object}  errorResponse  "collector_not_set — сбор статистики не настроен"
// @Router   /me/creator/projects/{id}/report/refresh [post]
func (h *Handler) CreatorRefreshStats(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	member, err := h.svc.CreatorInProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !member {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return
	}
	// Только свои ролики: чужие стоят кредитов, которых креатор не
	// тратил, и сдвигают чужое расписание сбора.
	h.refreshStats(w, r, projectID, &uid)
}

// ManagerReportCSV godoc
// @Summary  Выгрузка отчёта в CSV (менеджер)
// @Description Для тех, кому нужен файл, а не страница.
// @Tags     manager-publications
// @Produce  text/csv
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {string} string "CSV"
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не найден или чужой"
// @Router   /manager/projects/{id}/report.csv [get]
func (h *Handler) ManagerReportCSV(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	rep, err := h.svc.Report(r.Context(), projectID, ReportFilter{})
	if err != nil {
		writeErr(w, err)
		return
	}

	writeReportCSV(w, projectID, rep)
}

// writeReportCSV — выгрузка одной и той же таблицы для менеджера и для
// заказчика. Общая, потому что расходиться этим двум выгрузкам незачем:
// разное у них только право её получить.
func writeReportCSV(w http.ResponseWriter, projectID uuid.UUID, rep Report) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="report-`+projectID.String()+`.csv"`)
	// BOM: без него Excel открывает кириллицу кракозябрами, и выгрузка
	// становится бесполезной ровно для тех, кому она нужна.
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"ролик", "креатор", "площадка", "дата сдачи",
		"просмотры", "лайки", "комментарии", "прирост за сутки", "ER, %", "собрано"})
	for _, v := range rep.VideoRows {
		er := ""
		if v.ERPercent != nil {
			er = strconv.FormatFloat(*v.ERPercent, 'f', 2, 64)
		}
		collected := ""
		if v.CollectedAt != nil {
			collected = v.CollectedAt.Format(time.RFC3339)
		}
		// Прирост неизвестен — пустая клетка, а не ноль: ноль в Excel
		// сложат с остальными и получат заниженную сумму.
		growth := ""
		if v.Growth24h != nil {
			growth = strconv.FormatInt(*v.Growth24h, 10)
		}
		// Имя, а не uuid: выгрузку открывает человек в Excel.
		creator := v.CreatorName
		if creator == "" {
			creator = v.CreatorUserID.String()
		}
		_ = cw.Write([]string{
			v.URL,
			creator,
			v.Platform,
			v.SubmittedAt.Format(dateLayout),
			strconv.FormatInt(v.Views, 10),
			strconv.FormatInt(v.Likes, 10),
			strconv.FormatInt(v.Comments, 10),
			growth,
			er,
			collected,
		})
	}
}

// ClientReport godoc
// @Summary  Отчёт по проекту (заказчик)
// @Description Те же цифры, что у менеджера, без внутренней кухни.
// @Description Доступен, только если у проекта включён показ статистики.
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} Report
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш либо показ статистики отключён (ответ одинаковый намеренно)"
// @Router   /me/projects/{id}/report [get]
func (h *Handler) ClientReport(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	allowed, err := h.svc.ClientCanSeeStats(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !allowed {
		// Один и тот же ответ на «не ваш проект» и «статистика закрыта»:
		// разные коды позволили бы перебором узнать, какие проекты
		// существуют.
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Отчёт недоступен.")
		return
	}
	rep, err := h.svc.Report(r.Context(), projectID, ReportFilter{})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rep.ForClient())
}

// ClientReportCSV godoc
// @Summary  Выгрузка отчёта в CSV (заказчик)
// @Description Та же таблица, что у менеджера. Доступна по тому же правилу,
// @Description что и сам отчёт: при выключенном показе статистики — 404,
// @Description как и «не ваш проект».
// @Tags     client-publications
// @Produce  text/csv
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {string} string "CSV"
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не ваш или показ статистики выключен"
// @Router   /me/projects/{id}/report.csv [get]
func (h *Handler) ClientReportCSV(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	allowed, err := h.svc.ClientCanSeeStats(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !allowed {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Отчёт недоступен.")
		return
	}
	rep, err := h.svc.Report(r.Context(), projectID, ReportFilter{})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeReportCSV(w, projectID, rep.ForClient())
}

// CreatorReport godoc
// @Summary  Отчёт по своим роликам (креатор)
// @Description Только свои ролики: чужие цифры не попадают в выдачу.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} Report
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — вы не участник этого проекта"
// @Failure      410  {object}  errorResponse  "collapsed_no_detail — проект закрыт, подробная статистика по роликам больше не хранится"
// @Router   /me/creator/projects/{id}/report [get]
func (h *Handler) CreatorReport(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	member, err := h.svc.CreatorInProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !member {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return
	}
	rep, err := h.svc.Report(r.Context(), projectID, ReportFilter{CreatorUserID: &uid})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rep)
}

// ---- взгляд клиента ----

type clientFeedResp struct {
	Items []ClientVideo `json:"items"`
}

type calendarResp struct {
	Month string        `json:"month"`
	Days  []CalendarDay `json:"days"`
	// Months — месяцы (ГГГГ-ММ), в которых у проекта вообще есть
	// выкладки. Сетка показывает один месяц, и без этого списка пустой
	// месяц неотличим от «данные не доехали»: заказчик видел сентябрь с
	// одной точкой и не знал, что десять остальных выкладок в августе.
	Months []string `json:"months"`
}

// assertClient — проект принадлежит этому заказчику. Отдельная проверка
// от показа статистики: лента и календарь доступны всегда, цифры — по
// настройке проекта.
func (h *Handler) assertClient(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return uuid.Nil, uuid.Nil, false
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return uuid.Nil, uuid.Nil, false
	}
	owns, err := h.svc.ClientOwnsProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	if !owns {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return uuid.Nil, uuid.Nil, false
	}
	return projectID, uid, true
}

// ClientVideos godoc
// @Summary  Вышедшие ролики проекта (заказчик)
// @Description Ролик появляется в ленте, когда его можно посмотреть, — то есть
// @Description когда креатор сдал ссылки, а не когда стоит дата в плане.
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} clientFeedResp
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Router   /me/projects/{id}/videos [get]
func (h *Handler) ClientVideos(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ClientFeed(r.Context(), projectID, 50)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Лента доступна всегда — клиент вправе видеть, что ролики выходят.
	// А вот цифры в ней подчиняются той же настройке, что и отчёт: иначе
	// закрытая статистика утекала бы через эту ручку, и правило
	// «статистика по настройке» выполнялось бы только наполовину.
	allowed, err := h.svc.ClientCanSeeStats(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !allowed {
		for i := range items {
			items[i].Views, items[i].Likes, items[i].Comments = 0, 0, 0
			items[i].CollectedAt = nil
			items[i].StatsHidden = true
		}
	}
	httpx.WriteJSON(w, http.StatusOK, clientFeedResp{Items: items})
}

// ClientCalendar godoc
// @Summary  Календарь месяца (заказчик)
// @Description Что уже вышло и что запланировано, с отметкой креатора.
// @Description Просрочек в календаре нет: выкладка с прошедшей датой без
// @Description ссылок остаётся «запланированной».
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    month query string false "месяц в виде ГГГГ-ММ, по умолчанию текущий"
// @Description В ответе рядом с днями идёт список months — месяцы, в которых у
// @Description проекта вообще есть выкладки. Календарь показывает один месяц, и
// @Description без этого списка пустой месяц неотличим от потерянных данных.
// @Success  200 {object} calendarResp
// @Failure      400  {object}  errorResponse  "bad_id; bad_month — месяц не в формате ГГГГ-ММ"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Router   /me/projects/{id}/calendar [get]
func (h *Handler) ClientCalendar(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	month := time.Now().UTC()
	if raw := r.URL.Query().Get("month"); raw != "" {
		parsed, err := time.Parse("2006-01", raw)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month",
				"Месяц должен быть в формате ГГГГ-ММ.")
			return
		}
		month = parsed
	}
	days, err := h.svc.Calendar(r.Context(), projectID, month)
	if err != nil {
		writeErr(w, err)
		return
	}
	months, err := h.svc.CalendarMonths(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, calendarResp{
		Month: month.Format("2006-01"), Days: days, Months: months,
	})
}

type prefsReq struct {
	OnNewVideo     *bool  `json:"on_new_video"`
	OnWeeklyDigest *bool  `json:"on_weekly_digest"`
	OnDateShift    *bool  `json:"on_date_shift"`
	ViewsThreshold *int64 `json:"views_threshold"`
}

// ClientGetPrefs godoc
// @Summary  Настройки уведомлений (заказчик)
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} NotificationPrefs
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Router   /me/projects/{id}/notifications [get]
func (h *Handler) ClientGetPrefs(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	prefs, err := h.svc.Prefs(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, prefs)
}

// ClientSavePrefs godoc
// @Summary  Изменить настройки уведомлений (заказчик)
// @Description Незаданные поля не меняются. views_threshold = 0 или null
// @Description означает «не уведомлять о просмотрах».
// @Tags     client-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body prefsReq true "настройки уведомлений"
// @Success  200 {object} NotificationPrefs
// @Failure      400  {object}  errorResponse  "bad_json; bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Router   /me/projects/{id}/notifications [put]
func (h *Handler) ClientSavePrefs(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	var req prefsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	// Частичное обновление ОДНИМ запросом, без чтения перед записью:
	// иначе две вкладки клиента затирают изменения друг друга. Незаданное
	// поле сохраняет прежнее значение.
	patch := PrefsPatch{
		OnNewVideo:     req.OnNewVideo,
		OnWeeklyDigest: req.OnWeeklyDigest,
		OnDateShift:    req.OnDateShift,
	}
	if req.ViewsThreshold != nil {
		patch.ViewsThreshold = &req.ViewsThreshold
	}
	if err := h.svc.PatchPrefs(r.Context(), projectID, uid, patch); err != nil {
		writeErr(w, err)
		return
	}
	saved, err := h.svc.Prefs(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// errorResponse — тело ошибки, как его отдаёт httpx.
//
// В соседних доменах этот тип объявлен как одно поле `error`, но
// фактически WriteErrMsg кладёт ещё и `message` — человеческий текст,
// который показывает интерфейс. Описываем как есть: фронт живёт в
// отдельном репозитории и читает swagger, а не наш код.
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}
