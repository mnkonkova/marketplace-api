package projects

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// chiURLParam — обёртка для удобства, чтобы не тащить chi import в каждый
// handlers-файл. Использовалось в handlers_specialist.go.
func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}

// decodeOptionalJSON — для ручек, у которых тело опциональное (advance_stage,
// cancel-reason и т.п.). Пустое тело — нормально (возвращаем nil). Битый JSON —
// ошибка: иначе клиент мог бы тихо «потерять» optimistic-lock updated_at,
// прислав поломанное тело, и обойти проверку гонок.
func decodeOptionalJSON(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
