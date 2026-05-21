package feed

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// feedMaxIDs — потолок на количество user_id в /feed?ids=... Защита от
// «дай мне всех спецов в одном запросе» — выше этого фронт должен
// страничить (или использовать /feed без ids с обычными фильтрами).
const feedMaxIDs = 100

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Feed godoc
// @Summary      Лента «портфолио по категориям»
// @Description  Возвращает специалистов и связанные видео из портфолио. Поддерживает фильтры и cursor-пагинацию.
// @Tags         feed
// @Produce      json
// @Param        q              query  string  false  "search text (игнорируется если задан ids)"
// @Param        category       query  []string  false  "category codes (multi, csv; игнорируется если задан ids)"  collectionFormat(csv)
// @Param        skill          query  []string  false  "skill slugs (multi, csv; игнорируется если задан ids)"  collectionFormat(csv)
// @Param        city           query  string  false  "city (игнорируется если задан ids)"
// @Param        ids            query  []string  false  "user_id (csv, до 100) — жёсткий фильтр: показывать видео только этих спецов"  collectionFormat(csv)
// @Param        per_specialist query  int     false  "видео на специалиста"
// @Param        cursor         query  string  false  "cursor токен"
// @Success      200            {object}  Result
// @Failure      502            {object}  feedError
// @Router       /feed [get]
func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	q := parseQuery(r)
	res, err := h.svc.Feed(r.Context(), q)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "feed_unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func parseQuery(r *http.Request) Query {
	v := r.URL.Query()
	q := Query{
		Q:          strings.TrimSpace(v.Get("q")),
		Categories: splitCSV(v["category"]),
		SkillSlugs: splitCSV(v["skill"]),
		City:       strings.TrimSpace(v.Get("city")),
		Cursor:     strings.TrimSpace(v.Get("cursor")),
		UserIDs:    parseIDs(v["ids"]),
	}
	if s := v.Get("per_specialist"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			q.PerSpecialist = n
		}
	}
	return q
}

// parseIDs — CSV (или повторяющийся ?ids=) → []uuid.UUID. Мусор молча
// дропаем (на фронте легко передать одну пустую запятую — это не повод
// 400-ить весь запрос). Cap на feedMaxIDs защищает от выкачивания всей базы.
func parseIDs(values []string) []uuid.UUID {
	if len(values) == 0 {
		return nil
	}
	out := make([]uuid.UUID, 0, feedMaxIDs)
	seen := make(map[uuid.UUID]struct{}, feedMaxIDs)
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := uuid.Parse(part)
			if err != nil {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) >= feedMaxIDs {
				return out
			}
		}
	}
	return out
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

// feedError — форма ошибки для swaggo.
type feedError struct {
	Error string `json:"error"`
}
