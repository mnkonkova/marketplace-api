package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/httpx"
	"marketpclce/internal/projects"
)

// Начислений по людям у проекта без креаторов нет — и это не «пока нет».
//
// Пустой список начислений на денежном экране читается как «расчёт ещё не
// делали»: менеджер ждёт кнопку «пересчитать», заказчик — строки «Команда
// периода». Ждать нечего, поэтому ветка начислений отказывает явно.
//
// Сами условия проекта (GET/PUT /manager/projects/{id}/billing) при этом
// остаются доступны: в снимке условий лежит стоимость проекта, которую у
// этого вида называет менеджер, и другого места под неё нет.

// ProjectHasBilling — у вида проекта есть начисления по людям.
func (s *Service) ProjectHasBilling(ctx context.Context, projectID uuid.UUID) (bool, error) {
	var kind string
	err := s.repo.db.QueryRow(ctx, `SELECT kind::text FROM projects WHERE id = $1`, projectID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("get project kind: %w", err)
	}
	return projects.FeaturesOf(projects.ProjectKind(kind)).HasBilling, nil
}

// requireBilling — отказать 409, если начислений у вида не бывает.
func (h *Handler) requireBilling(w http.ResponseWriter, r *http.Request, projectID uuid.UUID) bool {
	ok, err := h.svc.ProjectHasBilling(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if !ok {
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_project_kind",
			"У проекта «бренд под ключ» начислений нет: платить и считать не по кому. "+
				"Стоимость такого проекта задаётся одним числом в его условиях.")
		return false
	}
	return true
}
