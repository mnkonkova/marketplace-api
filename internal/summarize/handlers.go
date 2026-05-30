package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"marketpclce/internal/httpx"
	"marketpclce/internal/llm"
	"marketpclce/internal/search"
)

type Handler struct {
	svc       *Service
	searchSvc *search.Service
	cache     *Cache
}

type HandlerConfig struct {
	Search *search.Service
	Cache  *Cache
}

func NewHandler(svc *Service, c HandlerConfig) *Handler {
	return &Handler{
		svc:       svc,
		searchSvc: c.Search,
		cache:     c.Cache,
	}
}

type request struct {
	Q              string   `json:"q"`
	Categories     []string `json:"categories"`
	Skills         []string `json:"skills"`
	City           string   `json:"city"`
	RateMin        *int     `json:"rate_min"`
	RateMax        *int     `json:"rate_max"`
	Limit          int      `json:"limit"`
	TargetCategory string   `json:"target_category"`
}

// Summarize godoc
// @Summary      LLM-подбор «топ-N специалистов»
// @Description  По текстовому запросу/фильтрам поиска возвращает подобранную LLM-выдачу.
// @Tags         llm
// @Accept       json
// @Produce      json
// @Param        body  body      request  true  "поисковые параметры"
// @Success      200   {object}  Result
// @Failure      400   {object}  errorResponse
// @Failure      429   {object}  errorResponse
// @Failure      502   {object}  errorResponse
// @Router       /search/summarize [post]
func (h *Handler) Summarize(w http.ResponseWriter, r *http.Request) {
	var in request
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON в теле запроса.")
		return
	}
	q := search.Query{
		Q:          strings.TrimSpace(in.Q),
		Categories: in.Categories,
		SkillSlugs: in.Skills,
		City:       strings.TrimSpace(in.City),
		RateMin:    in.RateMin,
		RateMax:    in.RateMax,
		Limit:      in.Limit,
	}

	// target_category: явный приоритет, фолбэк — единственная категория из фильтров.
	targetCategory := strings.TrimSpace(in.TargetCategory)
	if targetCategory == "" && len(in.Categories) == 1 {
		targetCategory = in.Categories[0]
	}

	if cached, ok := h.cache.Get(r.Context(), q); ok {
		cached.Cached = true
		h.attachCategoryTotal(r.Context(), &cached, targetCategory)
		httpx.WriteJSON(w, http.StatusOK, cached)
		return
	}

	res, err := h.svc.Run(r.Context(), q)
	if err != nil {
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			slog.Error("summarize llm api", "status", apiErr.Status, "body", apiErr.Body)
		} else {
			slog.Error("summarize", "err", err)
		}
		httpx.WriteErrMsg(w, http.StatusBadGateway, "summarize_failed",
			"LLM не смог собрать саммари по выдаче. Попробуйте позже.")
		return
	}

	h.cache.Set(r.Context(), q, res)
	h.attachCategoryTotal(r.Context(), &res, targetCategory)
	httpx.WriteJSON(w, http.StatusOK, res)
}

// attachCategoryTotal дорисовывает total_in_category. Ошибку count'а проглатываем —
// это вторичная информация, которая не должна валить весь LLM-подбор.
func (h *Handler) attachCategoryTotal(ctx context.Context, res *Result, targetCategory string) {
	if targetCategory == "" || h.searchSvc == nil {
		return
	}
	n, err := h.searchSvc.CountByCategory(ctx, targetCategory)
	if err != nil {
		slog.Warn("count by category", "code", targetCategory, "err", err)
		return
	}
	res.TargetCategory = targetCategory
	res.TotalInCategory = n
}

type errorResponse struct {
	Error string `json:"error"`
}
