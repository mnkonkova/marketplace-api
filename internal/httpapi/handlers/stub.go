package handlers

import "net/http"

func NotImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not_implemented"})
}
