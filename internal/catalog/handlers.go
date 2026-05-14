package catalog

import (
	"encoding/json"
	"net/http"
)

type Handler struct{ repo *Repo }

func NewHandler(repo *Repo) *Handler { return &Handler{repo: repo} }

// Categories godoc
// @Summary      Список категорий специалистов
// @Tags         catalog
// @Produce      json
// @Success      200  {object}  CategoriesResponse
// @Router       /categories [get]
func (h *Handler) Categories(w http.ResponseWriter, r *http.Request) {
	cats, err := h.repo.ListCategories(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": cats})
}

// Skills godoc
// @Summary      Список навыков
// @Tags         catalog
// @Produce      json
// @Param        kind  query     string  false  "tool|platform|genre"
// @Success      200   {object}  SkillsResponse
// @Failure      400   {object}  errorResponse
// @Router       /skills [get]
func (h *Handler) Skills(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "", "tool", "platform", "genre":
	default:
		writeErr(w, http.StatusBadRequest, "bad_kind")
		return
	}
	skills, err := h.repo.ListSkills(r.Context(), kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": skills})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// CategoriesResponse / SkillsResponse / errorResponse — нужны swaggo для типизации @Success/@Failure.
type CategoriesResponse struct {
	Items []Category `json:"items"`
}

type SkillsResponse struct {
	Items []Skill `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
