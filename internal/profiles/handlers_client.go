package profiles

import (
	"encoding/json"
	"errors"
	"net/http"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// GetClient godoc
// @Summary  Контактные данные клиента (display_name, phone, telegram)
// @Tags     client-profile
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} ClientProfile
// @Router   /me/client-profile [get]
func (h *Handler) GetClient(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	cp, err := h.svc.GetClientProfile(r.Context(), uid)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, cp)
}

// PatchClient godoc
// @Summary  Сохранить контакты клиента
// @Tags     client-profile
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body ClientProfilePatch true "patch"
// @Success  200  {object} ClientProfile
// @Router   /me/client-profile [patch]
func (h *Handler) PatchClient(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in ClientProfilePatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	cp, err := h.svc.PatchClientProfile(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		// data-sec D12: только детали обёртки, без префикса "invalid input:".
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at", msgStale)
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, cp)
	}
}
