package projects

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// chiURLParam — обёртка для удобства, чтобы не тащить chi import в каждый
// handlers-файл. Использовалось в handlers_specialist.go.
func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}
