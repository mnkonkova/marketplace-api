package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func parseID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id в URL.")
		return uuid.Nil, false
	}
	return id, true
}

func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Пользователь не найден.")
	case errors.Is(err, ErrNotManager):
		httpx.WriteErrMsg(w, http.StatusConflict, "not_manager",
			"Этот пользователь не имеет роли менеджера.")
	case errors.Is(err, ErrAlreadyExists):
		httpx.WriteErrMsg(w, http.StatusConflict, "already_exists",
			"Пользователь с таким email уже существует.")
	case errors.Is(err, ErrInviteInvalid):
		httpx.WriteErrMsg(w, http.StatusGone, "invite_invalid",
			"Ссылка инвайта недействительна или просрочена.")
	default:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// AdminListManagers godoc
// @Summary  Список менеджеров (с фильтром is_approved)
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    is_approved query bool false "true|false"
// @Success  200 {object} managersListResp
// @Router   /admin/managers [get]
func (h *Handler) AdminListManagers(w http.ResponseWriter, r *http.Request) {
	var approved *bool
	if v := strings.TrimSpace(r.URL.Query().Get("is_approved")); v != "" {
		b := v == "true" || v == "1"
		approved = &b
	}
	items, err := h.svc.ListManagers(r.Context(), approved)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managersListResp{Items: items})
}

// AdminApproveManager godoc
// @Summary  Аппрувить менеджера
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  204
// @Router   /admin/managers/{id}/approve [post]
func (h *Handler) AdminApproveManager(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.ApproveManager(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminRevokeManager godoc
// @Summary  Снять аппрув с менеджера
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  204
// @Router   /admin/managers/{id}/revoke [post]
func (h *Handler) AdminRevokeManager(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.RevokeManager(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createClientReq struct {
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
	GenerateInvite  bool   `json:"generate_invite"`
}

// AdminCreateClient godoc
// @Summary  Создать клиента вручную (опц. сгенерить инвайт)
// @Tags     admin-users
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body createClientReq true "client data"
// @Success  201 {object} CreateClientResult
// @Router   /admin/users [post]
func (h *Handler) AdminCreateClient(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	var in createClientReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	res, err := h.svc.CreateClient(r.Context(), CreateClientInput{
		Email: in.Email, DisplayName: in.DisplayName,
	}, in.GenerateInvite, actorID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

// AdminGenerateInvite godoc
// @Summary  Сгенерировать инвайт для существующего юзера
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  200 {object} InviteGenerateResult
// @Router   /admin/users/{id}/generate_invite [post]
func (h *Handler) AdminGenerateInvite(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	res, err := h.svc.GenerateInvite(r.Context(), id, actorID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type redeemResp struct {
	UserID string         `json:"user_id"`
	Tokens auth.TokenPair `json:"tokens"`
}

// RedeemInvite godoc
// @Summary  Обменять magic-link на JWT (публичный)
// @Tags     auth
// @Produce  json
// @Param    token path string true "raw invite token"
// @Success  200 {object} redeemResp
// @Failure  410 {object} errorResponse "invite_invalid"
// @Router   /auth/redeem_invite/{token} [post]
func (h *Handler) RedeemInvite(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	pair, userID, err := h.svc.RedeemInvite(r.Context(), token)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, redeemResp{
		UserID: userID.String(),
		Tokens: pair,
	})
}

// типы для swaggo
type managersListResp struct {
	Items []ManagerInfo `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
