package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"marketpclce/internal/httpx"
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

// Register godoc
// @Summary      Регистрация пользователя
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      registerReq  true  "регистрационные данные"
// @Success      201   {object}  registerResp
// @Failure      400   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /auth/register [post]
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var in registerReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
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
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrAlreadyExists):
		httpx.WriteErr(w, http.StatusConflict, "user_exists")
		return
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, registerResp{UserID: res.UserID.String(), Tokens: res.Tokens})
}

type loginReq struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Login godoc
// @Summary      Логин по email/телефону и паролю
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      loginReq  true  "credentials"
// @Success      200   {object}  TokenPair
// @Failure      401   {object}  errorResponse
// @Failure      403   {object}  errorResponse
// @Router       /auth/login [post]
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	pair, err := h.svc.Login(r.Context(), in.Login, in.Password)
	switch {
	case errors.Is(err, ErrBadCredentials):
		httpx.WriteErr(w, http.StatusUnauthorized, "bad_credentials")
		return
	case errors.Is(err, ErrInactive):
		httpx.WriteErr(w, http.StatusForbidden, "inactive")
		return
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pair)
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh godoc
// @Summary      Обмен refresh-токена на новую пару
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      refreshReq  true  "refresh token"
// @Success      200   {object}  TokenPair
// @Failure      401   {object}  errorResponse
// @Router       /auth/refresh [post]
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var in refreshReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	pair, err := h.svc.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		httpx.WriteErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pair)
}

type meResp struct {
	UserID string  `json:"user_id"`
	Email  *string `json:"email,omitempty"`
	Phone  *string `json:"phone,omitempty"`
	Kind   string  `json:"kind"`
}

// Me godoc
// @Summary      Текущий пользователь
// @Tags         auth
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  meResp
// @Failure      401  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /me [get]
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	uid, ok := UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	u, err := h.svc.GetUser(r.Context(), uid)
	if err != nil {
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meResp{UserID: u.ID.String(), Email: u.Email, Phone: u.Phone, Kind: u.Kind})
}


// errorResponse — стандартная форма ошибки `{ "error": "..." }`. Объявлено
// тут, чтобы swaggo подхватил тип в @Failure.
type errorResponse struct {
	Error string `json:"error"`
}
