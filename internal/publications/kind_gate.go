package publications

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

// Выключатели по виду проекта.
//
// Ручки, которых у вида не бывает, обязаны отказывать ЯВНО, а не отдавать
// пустоту. Пустой состав читается как «ещё никого не добавили», пустой
// чек-лист — как «забыли подключить», пустая проверка — как «не смотрели».
// У проекта «бренд под ключ» всего этого не будет никогда, и разница между
// «пока нет» и «не бывает» — это разница между «подожду» и «не жди».

// ProjectFeatures — что умеет вид этого проекта.
//
// Один короткий запрос на ручку. Дешевле, чем кажется: те же ручки уже
// ходят в projects за проверкой доступа, и ещё одно чтение строки по
// первичному ключу на фоне отчёта из пяти запросов не видно.
func (s *Service) ProjectFeatures(ctx context.Context, projectID uuid.UUID) (projects.KindFeatures, error) {
	return s.repo.ProjectFeatures(ctx, projectID)
}

// ProjectFeatures — см. Service.ProjectFeatures.
func (r *Repo) ProjectFeatures(ctx context.Context, projectID uuid.UUID) (projects.KindFeatures, error) {
	var kind string
	err := r.db.QueryRow(ctx, `SELECT kind::text FROM projects WHERE id = $1`, projectID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return projects.KindFeatures{}, ErrNotFound
	}
	if err != nil {
		return projects.KindFeatures{}, fmt.Errorf("get project kind: %w", err)
	}
	return projects.FeaturesOf(projects.ProjectKind(kind)), nil
}

// requireFeature — отказать 409, если блока у вида нет.
//
// Текст отказа обязан объяснять, ПОЧЕМУ блока нет: «недоступно» на
// экране, где кнопка только что была, читается как поломка прав.
func (h *Handler) requireFeature(
	w http.ResponseWriter, r *http.Request, projectID uuid.UUID,
	has func(projects.KindFeatures) bool, why string,
) bool {
	f, err := h.svc.ProjectFeatures(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if !has(f) {
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_project_kind", why)
		return false
	}
	return true
}

// Предикаты вынесены именами, а не литералами на месте вызова: так в
// списке ручек видно, какой блок какой ручкой закрыт.
func hasCrew(f projects.KindFeatures) bool      { return f.HasCrew }
func hasReview(f projects.KindFeatures) bool    { return f.HasReview }
func hasChecklist(f projects.KindFeatures) bool { return f.HasChecklist }

const (
	whyNoCrew = "У проекта «бренд под ключ» состава нет: ролики выходят с аккаунтов бренда, " +
		"и добавлять в него людей некуда."
	whyNoReview = "У проекта «бренд под ключ» проверки роликов нет: " +
		"проверять работу не у кого — креаторов в нём не бывает."
	whyNoChecklist = "У проекта «бренд под ключ» чек-листа нет: " +
		"требования к выкладке предъявлять некому."
)
