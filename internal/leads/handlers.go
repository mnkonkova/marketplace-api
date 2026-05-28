package leads

import (
	"encoding/json"
	"errors"
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

type createReq struct {
	ClientName     string   `json:"client_name"`
	ClientContact  string   `json:"client_contact"`
	Brief          string   `json:"brief"`
	BudgetMin      *int     `json:"budget_min"`
	BudgetMax      *int     `json:"budget_max"`
	Deadline       string   `json:"deadline"`
	TargetCategory string   `json:"target_category"`
	SpecialistIDs  []string `json:"specialist_ids"`
}

// invalidInputMessage — детали валидации без префикса "invalid input: ".
func invalidInputMessage(err error) string {
	const prefix = "invalid input: "
	s := err.Error()
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix)
	}
	return s
}

// Create godoc
// @Summary      Создать заявку (lead)
// @Description  Менеджер/клиент создаёт лид и выбирает специалистов-получателей. В ответе — id и контакты выбранных спецов (видны только создателю).
// @Tags         leads
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      createReq  true  "lead payload"
// @Success      201   {object}  CreateResult
// @Failure      400   {object}  errorResponse
// @Failure      403   {object}  errorResponse  "email_unverified — для авторизованного клиента email должен быть подтверждён"
// @Router       /leads [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in createReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON в теле запроса.")
		return
	}

	ids := make([]uuid.UUID, 0, len(in.SpecialistIDs))
	for i, raw := range in.SpecialistIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			httpx.WriteErrFields(w, http.StatusBadRequest, "bad_specialist_id",
				"В списке specialist_ids есть невалидный UUID.",
				httpx.FieldError{
					Field:   "specialist_ids[" + strconv.Itoa(i) + "]",
					Message: "Должен быть UUID специалиста",
				})
			return
		}
		ids = append(ids, id)
	}

	var deadline *time.Time
	if s := strings.TrimSpace(in.Deadline); s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			httpx.WriteErrFields(w, http.StatusBadRequest, "bad_deadline",
				"Поле deadline должно быть в формате YYYY-MM-DD.",
				httpx.FieldError{Field: "deadline", Message: "Ожидался формат YYYY-MM-DD"})
			return
		}
		deadline = &d
	}

	var clientUserID *uuid.UUID
	if uid, ok := auth.UserIDFrom(r.Context()); ok {
		clientUserID = &uid
	}

	res, err := h.svc.Create(r.Context(), CreateInput{
		ClientUserID:       clientUserID,
		ClientName:         in.ClientName,
		ClientContact:      in.ClientContact,
		Brief:              in.Brief,
		BudgetMin:          in.BudgetMin,
		BudgetMax:          in.BudgetMax,
		Deadline:           deadline,
		TargetCategoryCode: in.TargetCategory,
		SpecialistIDs:      ids,
	})
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case errors.Is(err, ErrNoSpecialists):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "no_valid_specialists",
			"Среди выбранных специалистов нет ни одного валидного получателя.")
	case errors.Is(err, ErrEmailUnverified):
		httpx.WriteErrMsg(w, http.StatusForbidden, "email_unverified",
			"Подтвердите email — на него отправлено письмо.")
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось создать заявку.")
	default:
		// Ответ включает контакты выбранных спецов — эту ветку видит ТОЛЬКО
		// менеджер/клиент, который только что создал заявку. В feed/search/
		// публичный профиль контакты не уезжают (см. profiles.PublicProfile).
		httpx.WriteJSON(w, http.StatusCreated, res)
	}
}

// ListIncoming godoc
// @Summary      Входящие заявки специалиста
// @Tags         leads
// @Produce      json
// @Security     BearerAuth
// @Param        status  query     string  false  "sent|viewed|accepted|declined"
// @Param        limit   query     int     false  "default 20"
// @Param        offset  query     int     false  "default 0"
// @Success      200     {object}  incomingListResponse
// @Failure      400     {object}  errorResponse
// @Failure      401     {object}  errorResponse
// @Router       /me/leads/incoming [get]
func (h *Handler) ListIncoming(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Требуется авторизация.")
		return
	}
	v := r.URL.Query()
	status := v.Get("status")
	limit := atoi(v.Get("limit"), 20)
	offset := atoi(v.Get("offset"), 0)

	items, err := h.svc.ListIncoming(r.Context(), uid, status, limit, offset)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить входящие заявки.")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type recipientReq struct {
	Status string `json:"status"`
	// UpdatedAt — optimistic-lock версия lead_recipients.updated_at,
	// прислана фронтом из последнего GET /me/leads/incoming
	// (поле recipient_updated_at). Несовпадение → 409.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// UpdateRecipient godoc
// @Summary      Обновить статус получателя по лиду
// @Tags         leads
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string        true  "lead id"
// @Param        body  body      recipientReq  true  "статус"
// @Success      200   {object}  recipientStatusResp
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/leads/{id}/recipient [patch]
func (h *Handler) UpdateRecipient(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Требуется авторизация.")
		return
	}
	leadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrFields(w, http.StatusBadRequest, "bad_lead_id",
			"Неверный id лида.",
			httpx.FieldError{Field: "id", Message: "Должен быть UUID"})
		return
	}
	var in recipientReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON в теле запроса.")
		return
	}
	err = h.svc.UpdateRecipientStatus(r.Context(), leadID, uid, in.Status, in.UpdatedAt)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case errors.Is(err, ErrRecipientMissing):
		httpx.WriteErrMsg(w, http.StatusNotFound, "recipient_not_found",
			"Вы не получатель этого лида.")
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at",
			"Лид был обновлён другим запросом. Перезагрузите данные.")
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось обновить статус.")
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": in.Status})
	}
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}


// типы для swaggo
type errorResponse struct {
	Error string `json:"error"`
}

type incomingListResponse struct {
	Items []IncomingLead `json:"items"`
}

type recipientStatusResp struct {
	Status string `json:"status"`
}
