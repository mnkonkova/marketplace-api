package search

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := parseQuery(r)
	res, err := h.svc.Search(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "search_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) CategoryStats(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.CategoryStats(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "search_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func parseQuery(r *http.Request) Query {
	v := r.URL.Query()
	q := Query{
		Q:          strings.TrimSpace(v.Get("q")),
		Categories: splitCSV(v["category"]),
		SkillSlugs: splitCSV(v["skill"]),
		City:       strings.TrimSpace(v.Get("city")),
	}
	if s := v.Get("rate_min"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.RateMin = &n
		}
	}
	if s := v.Get("rate_max"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.RateMax = &n
		}
	}
	if s := v.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.Limit = n
		}
	}
	if s := v.Get("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.Offset = n
		}
	}
	return q
}

func splitCSV(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
