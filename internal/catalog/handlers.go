package catalog

import (
	"net/http"

	"marketpclce/internal/httpx"
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
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить категории")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": cats})
}

// Skills godoc
// @Summary      Список навыков
// @Tags         catalog
// @Produce      json
// @Param        kind      query     string  false  "tool|platform|genre|skill"
// @Param        category  query     string  false  "Код категории из /categories — отфильтровать навыки, релевантные категории (см. skill_categories). Платформы при фильтре по категории не возвращаются."
// @Success      200       {object}  SkillsResponse
// @Failure      400       {object}  errorResponse
// @Router       /skills [get]
func (h *Handler) Skills(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "", "tool", "platform", "genre", "skill":
	default:
		httpx.WriteErrFields(w, http.StatusBadRequest, "bad_kind",
			"Параметр kind должен быть одним из: tool, platform, genre, skill",
			httpx.FieldError{Field: "kind", Message: "Допустимо: tool, platform, genre, skill или пусто"})
		return
	}
	category := r.URL.Query().Get("category")
	skills, err := h.repo.ListSkills(r.Context(), SkillFilter{Kind: kind, Category: category})
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить навыки")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": skills})
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
