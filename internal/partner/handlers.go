package partner

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type linkReq struct {
	Code string `json:"code"`
}

// Link — POST /api/v1/partner/telegram-link.
//
// Требует авторизации: смысл ручки в том, что аккаунт подтверждаем мы, а не
// человек своими словами. Отсюда и ответы: каждая причина отказа отдаётся
// отдельным кодом, потому что действия у них разные — подтвердить почту,
// вернуться в приложение за новым кодом или обратиться в поддержку.
func (h *Handler) Link(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Войдите, чтобы привязать аккаунт")
		return
	}

	var in linkReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "invalid_json")
		return
	}
	in.Code = strings.TrimSpace(in.Code)
	if in.Code == "" || len(in.Code) > 64 {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_code",
			"Ссылка неполная — откройте её из приложения заново")
		return
	}

	switch err := h.svc.Link(r.Context(), uid, in.Code); {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, ErrNotVerified):
		httpx.WriteErrMsg(w, http.StatusForbidden, "email_not_verified",
			"Сначала подтвердите почту — письмо со ссылкой мы уже отправляли при регистрации")
	case errors.Is(err, ErrRejected):
		httpx.WriteErrMsg(w, http.StatusConflict, "code_rejected",
			"Ссылка устарела или уже использована. Откройте «Привязать аккаунт» в приложении заново")
	case errors.Is(err, ErrDisabled):
		httpx.WriteErr(w, http.StatusServiceUnavailable, "disabled")
	default:
		httpx.WriteErrMsg(w, http.StatusBadGateway, "botrabot_unavailable",
			"Не дозвонились до Бота Работ. Попробуйте через минуту")
	}
}
