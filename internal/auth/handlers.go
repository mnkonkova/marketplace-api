package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"log/slog"
	"marketpclce/internal/httpx"
	"strings"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type registerReq struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	// Source — откуда пришла регистрация: "landing_clients" разрешает
	// авто-подтверждение email'а (юзер сразу может создать бриф без
	// клика по письму). Валидность source не проверяем — это UX-ярлык,
	// не security-gate: клиент всё равно должен указать реальный контакт
	// в самом брифе (client_contact), туда менеджер и напишет.
	Source string `json:"source,omitempty"`
}

type registerResp struct {
	UserID string    `json:"user_id"`
	Tokens TokenPair `json:"tokens"`
	// IsNew — аккаунт создан этим запросом. Фронт по нему решает, вести ли
	// в мастер профиля или сразу в кабинет.
	IsNew bool `json:"is_new,omitempty"`
	// Kind — настоящая роль аккаунта. Фронт ведёт по ней, а не по той, что
	// сам запросил: иначе заказчик попадал в кабинет специалиста.
	Kind string `json:"kind,omitempty"`
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
		Source:      in.Source,
	})
	switch {
	// Раньше ErrAlreadyExists и ErrInvalidInput возвращались с пустым
	// message для anti-enumeration (атакующий не отличит «email занят»
	// от «битый ввод»). На практике эта защита всё равно частичная —
	// 201 vs 400 различимы при корректном вводе — а UX страдал: юзер
	// видел `invalid_input` без подсказки что не так с паролем/email'ом.
	// Сейчас отдаём конкретную причину, защиту от enumeration оставляем
	// на rate-limit `auth` (10/мин per IP).
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
		return
	case errors.Is(err, ErrAlreadyExists):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input",
			"Пользователь с таким email уже зарегистрирован")
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
// @Failure      400   {object}  errorResponse  "invalid_input — в теле нет login или password"
// @Failure      401   {object}  errorResponse
// @Failure      403   {object}  errorResponse
// @Router       /auth/login [post]
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	// Пустое поле — это «в запросе не то», а не «пароль не тот».
	//
	// Раньше и тот, и другой случай отвечали одинаково: bad_credentials.
	// Клиент, который назвал поле `email` вместо `login`, получал «неверный
	// логин или пароль» — и шёл искать проблему в паролях. Один раз это
	// стоило четырёх переписанных вслепую хешей на общем стенде, прежде
	// чем кто-то открыл DTO ручки.
	//
	// Защиту от перебора это не ослабляет: логина в запросе нет, значит и
	// подсказать по нему нечего — мы не говорим, существует ли такой
	// пользователь.
	if strings.TrimSpace(in.Login) == "" || in.Password == "" {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input",
			"Укажите login и password.")
		return
	}
	pair, err := h.svc.Login(r.Context(), in.Login, in.Password)
	switch {
	case errors.Is(err, ErrBadCredentials):
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "bad_credentials", "Неверный логин или пароль")
		return
	case errors.Is(err, ErrInactive):
		httpx.WriteErrMsg(w, http.StatusForbidden, "inactive",
			"Аккаунт отключён. Напишите в поддержку, если это ошибка.")
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
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "invalid_token", "Сессия истекла — войдите снова")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pair)
}

type meResp struct {
	UserID string `json:"user_id"`
	// DisplayName — имя аккаунта. Заказчику показывать больше нечего:
	// профиля у него нет, а раньше кабинет встречал его пустотой.
	DisplayName   string  `json:"display_name"`
	Email         *string `json:"email,omitempty"`
	Phone         *string `json:"phone,omitempty"`
	Kind          string  `json:"kind"`
	EmailVerified bool    `json:"email_verified"`
	// IsManager / IsAdmin — флаги CRM-прав. Фронт по ним решает, какие
	// кабинеты показывать в шапке. Кабинеты client/specialist выводятся
	// из kind.
	IsManager bool `json:"is_manager"`
	IsAdmin   bool `json:"is_admin"`
	// IsApproved — для manager обязательный аппрув; фронт показывает
	// «ждёт аппрува» вместо кабинета.
	IsApproved bool `json:"is_approved"`
}

type yandexReq struct {
	Code string `json:"code"`
	// Kind — роль для НОВОГО пользователя. Для существующего игнорируется:
	// заказчик, вошедший через Яндекс, не должен вдруг стать специалистом.
	Kind string `json:"kind"`
}

// YandexLogin godoc
// @Summary      Вход и регистрация через Яндекс
// @Description  Принимает одноразовый code из redirect'а Яндекса, обменивает
// @Description  его на профиль и выдаёт пару токенов. Регистрация и вход —
// @Description  одна ручка: человек не должен помнить, заводил ли аккаунт.
// @Tags         auth
// @Param        input  body      yandexReq  true  "code из redirect_uri"
// @Success      200    {object}  registerResp
// @Failure      400    {object}  errorResponse
// @Failure      501    {object}  errorResponse
// @Router       /auth/yandex [post]
func (h *Handler) YandexLogin(w http.ResponseWriter, r *http.Request) {
	var in yandexReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Code) == "" {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", "Не получен код авторизации")
		return
	}

	res, err := h.svc.LoginWithYandex(r.Context(), strings.TrimSpace(in.Code), in.Kind)
	switch {
	case errors.Is(err, ErrYandexDisabled):
		httpx.WriteErrMsg(w, http.StatusNotImplemented, "yandex_disabled",
			"Вход через Яндекс не настроен")
	case errors.Is(err, ErrYandexExchange):
		// Детали (какой именно client_secret не подошёл) наружу не отдаём:
		// человеку они не помогут, а нам расскажут в логе.
		slog.Warn("yandex exchange failed", "err", err)
		httpx.WriteErrMsg(w, http.StatusBadRequest, "yandex_failed",
			"Не удалось войти через Яндекс. Попробуйте ещё раз.")
	case err != nil:
		slog.Error("yandex login", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, registerResp{
			UserID: res.UserID.String(),
			Tokens: res.Tokens,
			IsNew:  res.IsNew,
			Kind:   res.Kind,
		})
	}
}

// EmailAvailable godoc
// @Summary      Свободен ли email для регистрации
// @Tags         auth
// @Param        email  query  string  true  "Проверяемый адрес"
// @Success      200  {object}  emailAvailableResp
// @Router       /auth/email-available [get]
func (h *Handler) EmailAvailable(w http.ResponseWriter, r *http.Request) {
	taken, err := h.svc.EmailTaken(r.Context(), r.URL.Query().Get("email"))
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, emailAvailableResp{Available: !taken})
}

type emailAvailableResp struct {
	Available bool `json:"available"`
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
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return
	}
	u, err := h.svc.GetUser(r.Context(), uid)
	if err != nil {
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meResp{
		UserID:        u.ID.String(),
		DisplayName:   u.DisplayName,
		Email:         u.Email,
		Phone:         u.Phone,
		Kind:          u.Kind,
		EmailVerified: u.EmailVerifiedAt != nil,
		IsManager:     u.IsManager,
		IsAdmin:       u.IsAdmin,
		IsApproved:    u.IsApproved,
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
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
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
	// Message — человеческий текст для интерфейса. omitempty: часть ручек
	// зовёт httpx.WriteErr без текста, и в ответе поля тогда нет.
	Message string `json:"message,omitempty"`
}
