package clarify

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"marketpclce/internal/httpx"
	"marketpclce/internal/llm"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type request struct {
	Category string    `json:"category"`
	History  []Message `json:"history"`
}

// Clarify godoc
// @Summary      Уточняющий диалог по брифу
// @Description  LLM возвращает следующий вопрос или конечный поисковый запрос (done=true).
// @Tags         llm
// @Accept       json
// @Produce      json
// @Param        body  body      request  true  "история диалога"
// @Success      200   {object}  Result
// @Failure      400   {object}  errorResponse
// @Failure      502   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /clarify [post]
func (h *Handler) Clarify(w http.ResponseWriter, r *http.Request) {
	var in request
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON в теле запроса.")
		return
	}
	if len(in.History) == 0 {
		httpx.WriteErrFields(w, http.StatusBadRequest, "empty_history",
			"История диалога пустая — добавьте хотя бы одно сообщение пользователя.",
			httpx.FieldError{Field: "history", Message: "Должна содержать минимум одно сообщение"})
		return
	}

	res, err := h.svc.Run(r.Context(), Input{Category: in.Category, History: in.History})
	switch {
	case errors.Is(err, ErrLLMDisabled):
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "llm_disabled",
			"LLM-провайдер не настроен на сервере.")
		return
	case err != nil:
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			slog.Error("clarify llm api", "status", apiErr.Status, "body", apiErr.Body)
		} else {
			slog.Error("clarify", "err", err)
		}
		// Graceful fallback: LLM споткнулся (битый JSON, таймаут, парс-ошибка).
		// Вместо 502 отдаём autopilot — done=true с последним сообщением юзера
		// в q, плюс хинт на категорию если она уже выбрана. Для пользователя
		// это «AI решил без уточнения» — лучше чем красная плашка в чате.
		last := lastUserText(in.History)
		if last != "" {
			fallback := Result{
				Message: "Принял задачу. Запускаю поиск.",
				Done:    true,
				Search:  &SearchParams{Q: last},
			}
			if in.Category != "" {
				fallback.Search.Categories = []string{in.Category}
			}
			httpx.WriteJSON(w, http.StatusOK, fallback)
			return
		}
		httpx.WriteErrMsg(w, http.StatusBadGateway, "clarify_failed",
			"LLM не смог обработать запрос. Попробуйте перефразировать.")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func lastUserText(history []Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			t := strings.TrimSpace(history[i].Content)
			if t != "" {
				return t
			}
		}
	}
	return ""
}

type errorResponse struct {
	Error string `json:"error"`
}
