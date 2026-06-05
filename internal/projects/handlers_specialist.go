package projects

import (
	"net/http"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// SpecialistList godoc
// @Summary  Назначенные мне проекты (специалист)
// @Tags     specialist-projects
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} managerListResp
// @Router   /me/specialist/projects [get]
func (h *Handler) SpecialistList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	items, err := h.svc.ListBySpecialist(r.Context(), uid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managerListResp{Items: items})
}

// SpecialistGetFunnel godoc
// @Summary  Воронка моего проекта (специалист, read-only)
// @Tags     specialist-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} ProjectFullView
// @Router   /me/specialist/projects/{id}/funnel [get]
func (h *Handler) SpecialistGetFunnel(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	pid, err := uuid.Parse(chiURLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	// Проверка: специалист — назначенный исполнитель?
	p, err := h.svc.repo.GetByID(r.Context(), pid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	if p.SpecialistUserID == nil || *p.SpecialistUserID != uid {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден или не назначен вам.")
		return
	}
	view, err := h.svc.GetFull(r.Context(), pid, uuid.Nil)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	// data-sec D4: GetFull собирает менеджерскую вьюху (Notes — внутренние
	// заметки + все шаги, включая visible_to_specialist=false). Спецу эти
	// поля показывать нельзя — пост-фильтрация на месте, без новой
	// репо-функции и переменной БД-цены.
	redactForSpecialist(&view)
	httpx.WriteJSON(w, http.StatusOK, view)
}

// redactForSpecialist — вычищает из ProjectFullView менеджерские поля
// перед отправкой специалисту. Скрытые шаги (visible_to_specialist=false)
// убираются полностью; display_status/прогресс пересчитываются по
// видимому подмножеству, чтобы цифры на UI соответствовали тому, что
// спец реально видит.
func redactForSpecialist(view *ProjectFullView) {
	view.Notes = ""
	allVisible := make([]StepView, 0)
	for i := range view.Stages {
		stage := &view.Stages[i]
		kept := stage.Steps[:0]
		for _, st := range stage.Steps {
			if st.VisibleToSpecialist {
				kept = append(kept, st)
			}
		}
		stage.Steps = kept
		ds, done, total := DeriveStageDisplayStatus(kept)
		stage.DisplayStatus = ds
		stage.StepsTotal = total
		stage.StepsDone = done
		allVisible = append(allVisible, kept...)
	}
	view.DisplayStatus = DeriveProjectDisplayStatus(view.Status, allVisible)
	view.Progress = DeriveProgress(allVisible)
	// CurrentStep* могут указывать на скрытый шаг — пересчёт по
	// видимому подмножеству.
	if cur := DeriveCurrentStep(allVisible); cur != nil {
		view.CurrentStepID = &cur.ID
		view.CurrentStepTitle = cur.Name
		view.CurrentStepOwner = cur.Owner
		view.CurrentStepStatus = cur.Status
	} else {
		view.CurrentStepID = nil
		view.CurrentStepTitle = ""
		view.CurrentStepOwner = ""
		view.CurrentStepStatus = ""
	}
}
