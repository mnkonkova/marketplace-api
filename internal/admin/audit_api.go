package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/audit"
	"marketpclce/internal/httpx"
)

// Журнал админских действий: чтение. Запись живёт в internal/audit и
// идёт в транзакции самого действия.

// WithAuditRepo подключает журнал. nil-safe: без вызова /admin/audit
// отдаёт пустую страницу, а карточка человека — пустой список записей.
// Так собранный без журнала сервис остаётся рабочим, а не падает 500-й
// на каждой карточке.
func (s *Service) WithAuditRepo(a *audit.Repo) *Service {
	s.audit = a
	return s
}

// AuditResult — страница журнала.
type AuditResult struct {
	Items  []audit.Entry `json:"items"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// ListAudit — страница журнала под фильтрами.
func (s *Service) ListAudit(ctx context.Context, f audit.Filter) (AuditResult, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	if s.audit == nil {
		return AuditResult{Items: []audit.Entry{}, Limit: limit, Offset: offset}, nil
	}
	items, total, err := s.audit.List(ctx, f)
	if err != nil {
		if strings.HasPrefix(err.Error(), "invalid ") {
			return AuditResult{}, wrapInvalid(err)
		}
		return AuditResult{}, err
	}
	return AuditResult{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// AdminListAudit godoc
// @Summary  Журнал админских действий
// @Description Фильтры: actor (uuid сотрудника), action (точный код,
// @Description например user.revoke_manager), object_type (user | project |
// @Description terms_version | checklist_template), object_id, from/to
// @Description (RFC3339, полуинтервал). Свежие сверху.
// @Tags     admin-audit
// @Produce  json
// @Security BearerAuth
// @Param    actor       query string false "uuid сотрудника"
// @Param    action      query string false "код действия"
// @Param    object_type query string false "user | project | terms_version | checklist_template"
// @Param    object_id   query string false "id объекта"
// @Param    from        query string false "RFC3339, включительно"
// @Param    to          query string false "RFC3339, не включая"
// @Param    limit       query int    false "1-200, default 50"
// @Param    offset      query int    false "default 0"
// @Success  200 {object} AuditResult
// @Failure  400 {object} errorResponse "bad_actor | bad_time | invalid_input"
// @Router   /admin/audit [get]
func (h *Handler) AdminListAudit(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, _ := strconv.Atoi(qs.Get("limit"))
	offset, _ := strconv.Atoi(qs.Get("offset"))
	f := audit.Filter{
		Action:     strings.TrimSpace(qs.Get("action")),
		ObjectType: strings.TrimSpace(qs.Get("object_type")),
		ObjectID:   strings.TrimSpace(qs.Get("object_id")),
		Limit:      limit,
		Offset:     offset,
	}
	if v := strings.TrimSpace(qs.Get("actor")); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_actor", "actor должен быть UUID.")
			return
		}
		f.ActorID = &id
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		v := strings.TrimSpace(qs.Get(p.name))
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_time",
				p.name+" должен быть датой в формате RFC3339.")
			return
		}
		*p.dst = &t
	}
	res, err := h.svc.ListAudit(r.Context(), f)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
