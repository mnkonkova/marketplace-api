package invites

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// TokenIssuer — узкий контракт выдачи JWT-пары. Реализуется
// *auth.TokenIssuer (тот же тип, что в auth-домене). Не вызываем
// auth напрямую, чтобы invites остался изолированным.
type TokenIssuer interface {
	Issue(userID uuidLike, now time.Time) (auth.TokenPair, error)
}

// uuidLike — workaround чтобы не тащить uuid в этот публичный
// интерфейс; auth.TokenIssuer.Issue принимает uuid.UUID, но adapter
// в main.go подгоняет сигнатуру.
type uuidLike interface {
	String() string
}

// RedeemHandler — публичный POST /auth/redeem_invite/{token}.
// Без auth: токен сам является аутентификатором.
type RedeemHandler struct {
	svc    *Service
	issuer *auth.TokenIssuer
}

func NewRedeemHandler(svc *Service, issuer *auth.TokenIssuer) *RedeemHandler {
	return &RedeemHandler{svc: svc, issuer: issuer}
}

// RedeemResponse — успешный ответ: пара токенов (как при логине).
type RedeemResponse struct {
	Tokens auth.TokenPair `json:"tokens"`
}

// Redeem godoc
// @Summary      Погасить magic-link инвайт (публичный)
// @Description  Принимает compound-токен из email-ссылки, помечает invite использованным, выставляет users.email_verified_at, выдаёт JWT-пару (как при логине).
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        token  path      string  true  "compound invite token"
// @Success      200    {object}  RedeemResponse
// @Failure      400    {object}  errorResponse "invalid|expired|used"
// @Router       /auth/redeem_invite/{token} [post]
func (h *RedeemHandler) Redeem(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_token", "Токен обязателен")
		return
	}
	ctx := context.Background()
	_ = ctx
	userID, err := h.svc.Redeem(r.Context(), token)
	switch {
	case errors.Is(err, ErrBadToken), errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_token", "Ссылка некорректна")
		return
	case errors.Is(err, ErrExpired):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "expired", "Срок действия ссылки истёк")
		return
	case errors.Is(err, ErrUsed):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "already_used", "Ссылкой уже воспользовались")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось активировать ссылку")
		return
	}

	pair, err := h.issuer.Issue(userID, time.Now())
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось выдать токен")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RedeemResponse{Tokens: pair})
}
