package httpx

import (
	"encoding/json"
	"net/http"
)

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// FieldError — детали по конкретному полю запроса. Используется в
// валидации DTO, чтобы фронт мог подсветить нужный input.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// errorBody — единый JSON-формат ответа об ошибке.
//
//	error    — стабильный машинный код (как раньше); фронт ветвится по нему.
//	message  — человекочитаемое сообщение для UI; опционально.
//	details  — список field-errors для валидации; опционально.
//
// Поля, которые не заданы, не попадают в JSON (omitempty), поэтому старые
// ответы остаются ровно такими же, какими были при WriteErr.
type errorBody struct {
	Error   string       `json:"error"`
	Message string       `json:"message,omitempty"`
	Details []FieldError `json:"details,omitempty"`
}

// WriteErr — back-compat обёртка. Возвращает {"error": code} без message.
// Использовать там, где код сам по себе достаточно говорящий, либо там, где
// нельзя раскрывать причину (anti-enumeration в /auth/register|/auth/login).
func WriteErr(w http.ResponseWriter, status int, code string) {
	WriteJSON(w, status, errorBody{Error: code})
}

// WriteErrMsg — ошибка с человеческим текстом для UI:
//
//	{"error": code, "message": msg}.
func WriteErrMsg(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, errorBody{Error: code, Message: msg})
}

// WriteErrFields — ошибка валидации с разбивкой по полям:
//
//	{"error": code, "message": msg, "details": [{field, message}, ...]}.
func WriteErrFields(w http.ResponseWriter, status int, code, msg string, fields ...FieldError) {
	WriteJSON(w, status, errorBody{Error: code, Message: msg, Details: fields})
}
