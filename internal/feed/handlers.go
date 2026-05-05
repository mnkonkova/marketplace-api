package feed

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	q := parseQuery(r)
	res, err := h.svc.Feed(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "feed_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func parseQuery(r *http.Request) Query {
	v := r.URL.Query()
	q := Query{
		Q:          strings.TrimSpace(v.Get("q")),
		Categories: splitCSV(v["category"]),
		SkillSlugs: splitCSV(v["skill"]),
		City:       strings.TrimSpace(v.Get("city")),
		Cursor:     strings.TrimSpace(v.Get("cursor")),
	}
	if s := v.Get("per_specialist"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.PerSpecialist = n
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
