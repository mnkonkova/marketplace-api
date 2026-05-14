package profilecheck

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/llm"
)

type ProfileLookup interface {
	PrimaryCategory(ctx context.Context, userID uuid.UUID) (code, title string, err error)
}

type Handler struct {
	svc     *Service
	lookup  ProfileLookup
}

func NewHandler(svc *Service, lookup ProfileLookup) *Handler {
	return &Handler{svc: svc, lookup: lookup}
}

type checkReq struct {
	Bio         string `json:"bio"`
	DisplayName string `json:"display_name"`
}

// Check godoc
// @Summary      Проверить bio/имя профиля через LLM
// @Description  LLM возвращает вердикты и подсказки по тексту bio и display_name. Доступен, только если включён LLM провайдер.
// @Tags         profile
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      checkReq  true  "профильные поля"
// @Success      200   {object}  Result
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      502   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/profile/check [post]
func (h *Handler) Check(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in checkReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}

	var code, title string
	if h.lookup != nil {
		c, t, err := h.lookup.PrimaryCategory(r.Context(), uid)
		if err == nil {
			code, title = c, t
		}
	}

	res, err := h.svc.Check(r.Context(), Input{
		Bio:                  in.Bio,
		DisplayName:          in.DisplayName,
		PrimaryCategory:      code,
		PrimaryCategoryTitle: title,
	})
	switch {
	case errors.Is(err, ErrEmptyInput):
		writeErr(w, http.StatusBadRequest, "empty_input")
		return
	case errors.Is(err, ErrLLMDisabled):
		writeErr(w, http.StatusServiceUnavailable, "llm_disabled")
		return
	case err != nil:
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			slog.Error("profilecheck llm api", "status", apiErr.Status, "body", apiErr.Body)
		} else {
			slog.Error("profilecheck", "err", err)
		}
		writeErr(w, http.StatusBadGateway, "check_failed")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

type errorResponse struct {
	Error string `json:"error"`
}
