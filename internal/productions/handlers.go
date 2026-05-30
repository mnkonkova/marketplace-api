package productions

import (
	"net/http"

	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// List godoc
// @Summary      Список активных продакшенов
// @Description  Публичный справочник для выпадающего списка в профиле специалиста. Возвращает только is_active=true. Сортировка по имени (case-insensitive).
// @Tags         productions
// @Produce      json
// @Success      200  {object}  ProductionsResponse
// @Failure      500  {object}  errorResponse
// @Router       /productions [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListActive(r.Context())
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить продакшены")
		return
	}
	out := make([]PublicProduction, 0, len(items))
	for _, p := range items {
		out = append(out, PublicProduction{ID: p.ID, Name: p.Name})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ProductionsResponse — обёртка для swagger типизации публичного списка.
type ProductionsResponse struct {
	Items []PublicProduction `json:"items"`
}

// AdminProductionsResponse — admin-список с полными полями.
type AdminProductionsResponse struct {
	Items []Production `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
