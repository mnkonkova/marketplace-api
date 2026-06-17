package httpx

import "context"

// RequestLog — изменяемая «корзина» полей для одной HTTP-обработки.
// Middleware вверху цепочки кладёт указатель в context, downstream'ы
// (auth, handlers) дописывают user_id / reason. Logger в defer читает
// финальное состояние и пишет в slog.
//
// Структура — указатель (а не значение в context), чтобы downstream
// мутировал то же место, а не свою копию.
type RequestLog struct {
	UserID string // UUID юзера если аутентифицирован
	Reason string // короткий машинный код ошибки (invalid_token, no_user…)
}

type reqLogKey struct{}

// WithReqLog кладёт пустой RequestLog в context и возвращает новый ctx.
// Вызывать ровно один раз — в request-logger middleware.
func WithReqLog(ctx context.Context) (context.Context, *RequestLog) {
	rl := &RequestLog{}
	return context.WithValue(ctx, reqLogKey{}, rl), rl
}

// ReqLog возвращает указатель на RequestLog из ctx или nil если его нет
// (например, если WithReqLog не был вызван — в тестах). Safe to call.
func ReqLog(ctx context.Context) *RequestLog {
	if rl, ok := ctx.Value(reqLogKey{}).(*RequestLog); ok {
		return rl
	}
	return nil
}

// SetReqUserID — хелпер для auth-middleware: записать user_id после
// успешного парсинга токена. No-op если RequestLog нет в ctx.
func SetReqUserID(ctx context.Context, userID string) {
	if rl := ReqLog(ctx); rl != nil {
		rl.UserID = userID
	}
}

// SetReqReason — хелпер для middleware/handlers: записать короткий код
// причины ошибки (тот же что идёт в "error" поле response).
// No-op если RequestLog нет в ctx.
func SetReqReason(ctx context.Context, reason string) {
	if rl := ReqLog(ctx); rl != nil {
		rl.Reason = reason
	}
}
