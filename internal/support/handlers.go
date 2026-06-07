package support

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type createReq struct {
	FromEmail string `json:"from_email"`
	FromName  string `json:"from_name,omitempty"`
	Topic     string `json:"topic"`
	Message   string `json:"message"`
	SourceURL string `json:"source_url,omitempty"`
}

type createResp struct {
	ID string `json:"id"`
}

// Create — POST /api/v1/support/messages. Открыт для гостей; если есть
// JWT-токен в Authorization — берём user_id оттуда (для трейса в БД).
// Email из тела всё равно обязательный — для гостей это их email, для
// залогиненых может отличаться от auth-email.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in createReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	var userID *uuid.UUID
	if uid, ok := auth.UserIDFrom(r.Context()); ok {
		userID = &uid
	}
	id, err := h.svc.Create(r.Context(), CreateInput{
		UserID:    userID,
		FromEmail: in.FromEmail,
		FromName:  in.FromName,
		Topic:     in.Topic,
		Message:   in.Message,
		SourceURL: in.SourceURL,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidEmail):
			writeErr(w, http.StatusBadRequest, "invalid email")
		case errors.Is(err, ErrInvalidMessage):
			writeErr(w, http.StatusBadRequest, "message must be 10-2000 chars")
		default:
			writeErr(w, http.StatusInternalServerError, "internal")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(createResp{ID: id.String()})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
