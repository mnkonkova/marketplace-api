package auth

import (
	"encoding/json"
	"errors"
	"net/http"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type registerReq struct {
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Password    string `json:"password"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
}

type registerResp struct {
	UserID string    `json:"user_id"`
	Tokens TokenPair `json:"tokens"`
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var in registerReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	res, err := h.svc.Register(r.Context(), RegisterInput{
		Email:       in.Email,
		Phone:       in.Phone,
		Password:    in.Password,
		Kind:        in.Kind,
		DisplayName: in.DisplayName,
	})
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrAlreadyExists):
		writeErr(w, http.StatusConflict, "user_exists")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, registerResp{UserID: res.UserID.String(), Tokens: res.Tokens})
}

type loginReq struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	pair, err := h.svc.Login(r.Context(), in.Login, in.Password)
	switch {
	case errors.Is(err, ErrBadCredentials):
		writeErr(w, http.StatusUnauthorized, "bad_credentials")
		return
	case errors.Is(err, ErrInactive):
		writeErr(w, http.StatusForbidden, "inactive")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, pair)
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var in refreshReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	pair, err := h.svc.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}
	writeJSON(w, http.StatusOK, pair)
}

type meResp struct {
	UserID string  `json:"user_id"`
	Email  *string `json:"email,omitempty"`
	Phone  *string `json:"phone,omitempty"`
	Kind   string  `json:"kind"`
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	uid, ok := UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	u, err := h.svc.GetUser(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, meResp{UserID: u.ID.String(), Email: u.Email, Phone: u.Phone, Kind: u.Kind})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
