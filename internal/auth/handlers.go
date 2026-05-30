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
// @Description  При невалидном вводе или занятом email отвечает 400 invalid_input
// @Description  (разные причины не различаются — anti-enumeration).
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      registerReq  true  "регистрационные данные"
// @Success      201   {object}  registerResp
// @Failure      400   {object}  errorResponse
// @Router       /auth/register [post]
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var in registerReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	res, err := h.svc.Register(r.Context(), RegisterInput{
		Email:       in.Email,
		Password:    in.Password,
		Kind:        in.Kind,
		DisplayName: in.DisplayName,
	})
	switch {
	// ErrAlreadyExists и ErrInvalidInput возвращаем одним статусом и кодом,
	// чтобы атакующий не мог по 409 vs 400 перебирать список зарегистрированных
	// email'ов. Конкретика (формат, длина пароля, занято) уходит во внутренний
	// лог, наружу — generic invalid_input. Anti-enumeration частичная: 201 vs
	// 400 различимы при корректном вводе, поэтому работает в паре с RL на
	// /auth/* (10/мин per IP).
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrAlreadyExists):
		httpx.WriteErr(w, http.StatusBadRequest, "invalid_input")
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
	UserID        string  `json:"user_id"`
	Email         *string `json:"email,omitempty"`
	Phone         *string `json:"phone,omitempty"`
	Kind          string  `json:"kind"`
	EmailVerified bool    `json:"email_verified"`
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
	httpx.WriteJSON(w, http.StatusOK, meResp{
		UserID:        u.ID.String(),
		Email:         u.Email,
		Phone:         u.Phone,
		Kind:          u.Kind,
		EmailVerified: u.EmailVerifiedAt != nil,
	})
}

type verifyEmailReq struct {
	Token string `json:"token"`
}

// VerifyEmail godoc
// @Summary      Подтвердить email по токену из письма
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      verifyEmailReq  true  "token из ссылки в письме"
// @Success      200   {object}  TokenPair       "новая пара токенов с актуальным email_verified"
// @Failure      400   {object}  errorResponse
// @Failure      410   {object}  errorResponse   "token_invalid: токен неизвестен, использован, просрочен или email сменился"
// @Router       /auth/verify-email [post]
func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var in verifyEmailReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	pair, err := h.svc.VerifyEmail(r.Context(), in.Token)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, "empty_token")
	case errors.Is(err, ErrTokenInvalid):
		httpx.WriteErr(w, http.StatusGone, "token_invalid")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, pair)
	}
}

// ResendVerification godoc
// @Summary      Перевыслать письмо подтверждения email
// @Description  Гасит прошлые токены и шлёт новое письмо. Cooldown 60s по user_id.
// @Tags         auth
// @Produce      json
// @Security     BearerAuth
// @Success      204
// @Failure      401  {object}  errorResponse
// @Failure      429  {object}  errorResponse  "resend_cooldown"
// @Router       /auth/resend-verification [post]
func (h *Handler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	uid, ok := UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	err := h.svc.ResendVerification(r.Context(), uid)
	switch {
	case errors.Is(err, ErrResendCooldown):
		httpx.WriteErr(w, http.StatusTooManyRequests, "resend_cooldown")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, "no_email")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type passwordResetRequestReq struct {
	Email string `json:"email"`
}

// RequestPasswordReset godoc
// @Summary      Запросить ссылку сброса пароля
// @Description  Всегда 204 (anti-enumeration). Если email зарегистрирован,
// @Description  на него уйдёт письмо со ссылкой DOMAIN/auth/reset?token=...
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      passwordResetRequestReq  true  "email"
// @Success      204
// @Failure      400   {object}  errorResponse
// @Router       /auth/password-reset/request [post]
func (h *Handler) RequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var in passwordResetRequestReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	// Внутренние ошибки не разглашаем; на anti-enumeration работает тот же
	// принцип что у Register — наружу всегда 204 если ввод не битый.
	if err := h.svc.RequestPasswordReset(r.Context(), in.Email); err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordResetConfirmReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// ConfirmPasswordReset godoc
// @Summary      Применить новый пароль по токену из письма
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      passwordResetConfirmReq  true  "token + новый пароль"
// @Success      200   {object}  TokenPair                "свежая пара tokens — фронт может авто-логинить"
// @Failure      400   {object}  errorResponse
// @Failure      410   {object}  errorResponse            "token_invalid: токен неизвестен, использован или просрочен"
// @Router       /auth/password-reset/confirm [post]
func (h *Handler) ConfirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var in passwordResetConfirmReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	pair, err := h.svc.ConfirmPasswordReset(r.Context(), in.Token, in.Password)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", "Пароль должен быть не короче 8 символов.")
	case errors.Is(err, ErrTokenInvalid):
		httpx.WriteErrMsg(w, http.StatusGone, "token_invalid", "Ссылка устарела или уже использована — закажите новую.")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, pair)
	}
}

// errorResponse — стандартная форма ошибки `{ "error": "..." }`. Объявлено
// тут, чтобы swaggo подхватил тип в @Failure.
type errorResponse struct {
	Error string `json:"error"`
}
