package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// Возврат отменённого проекта.
//
// Отмена — единственное «удаление» проекта в CRM, и промахнуться в ней
// легко: строки в списке стоят вплотную, подтверждения нет. До сих пор
// вернуть проект можно было только через psql, причём угадав, в каком
// статусе он был: отмена статус не запоминала нигде, кроме события.
//
// Статус берём именно из payload события project_cancelled — другого
// места, где он сохранился, нет.

var (
	// ErrNotCancelled — вернуть можно только отменённый проект.
	ErrNotCancelled = errors.New("project is not cancelled")
	// ErrNoCancelEvent — проект отменён, но события с прежним статусом
	// нет (отмена до этой версии либо правка статуса руками в БД).
	// Гадать не будем: восстановленный «не в тот» статус хуже отказа —
	// он молча меняет, кого проект ждёт.
	ErrNoCancelEvent = errors.New("no project_cancelled event to restore from")
)

// RestoreProject — вернуть отменённый проект в статус, который был до
// отмены. Возвращает восстановленный статус.
//
// Событие и outbox — как у отмены: n8n должен узнать о возврате тем же
// путём, которым узнал об отмене, иначе у проекта в чужих системах
// останется «отменён».
func (r *Repo) RestoreProject(ctx context.Context, projectID, actorID uuid.UUID) (ProjectStatus, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus ProjectStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&currentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("load status: %w", err)
	}
	if currentStatus != ProjectStatusCancelled {
		return "", ErrNotCancelled
	}

	// Последнее событие отмены, а не первое: проект могли отменить,
	// вернуть и отменить снова — вернуть надо туда, откуда ушли в
	// последний раз.
	var prev string
	err = tx.QueryRow(ctx, `
SELECT COALESCE(payload->>'from_status', '')
FROM project_step_events
WHERE project_id = $1 AND event_kind = 'project_cancelled'
ORDER BY created_at DESC, id DESC
LIMIT 1`, projectID).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoCancelEvent
	}
	if err != nil {
		return "", fmt.Errorf("load cancel event: %w", err)
	}
	restored := ProjectStatus(prev)
	if !isRestorableStatus(restored) {
		return "", ErrNoCancelEvent
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET status=$2, updated_at=now() WHERE id=$1`,
		projectID, string(restored)); err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	// from_status/to_status — enum шагов, статус проекта туда не лезет
	// (см. миграцию 00010 и CancelProject): кладём в payload.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'project_restored', $3)`,
		projectID, actorID,
		mustJSON(map[string]string{
			"from_status": string(ProjectStatusCancelled),
			"to_status":   string(restored),
		})); err != nil {
		return "", fmt.Errorf("event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.restored",
		map[string]string{
			"project_id": projectID.String(),
			"to_status":  string(restored),
		}); err != nil {
		return "", err
	}
	if err := audit.Write(ctx, tx, actorID, audit.ActionProjectRestore,
		audit.ObjectProject, projectID.String(),
		map[string]any{"to_status": string(restored)}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return restored, nil
}

// isRestorableStatus — в какой статус возврат осмыслен. cancelled в
// payload означал бы отмену отменённого (такого CancelProject не пишет),
// а незнакомая строка — испорченное событие.
func isRestorableStatus(s ProjectStatus) bool {
	switch s {
	case ProjectStatusDraft, ProjectStatusActive, ProjectStatusOnHold,
		ProjectStatusDone, ProjectStatusDispute:
		return true
	}
	return false
}

// RestoreProject — вернуть отменённый проект (админская операция).
func (s *Service) RestoreProject(ctx context.Context, projectID, actorID uuid.UUID) (ProjectStatus, error) {
	return s.repo.RestoreProject(ctx, projectID, actorID)
}

// restoreResp — новый статус проекта: фронту нужно понять, в какую
// колонку канбана карточка вернулась.
type restoreResp struct {
	Status ProjectStatus `json:"status"`
}

// AdminRestoreProject godoc
// @Summary  Вернуть отменённый проект
// @Description Проект возвращается в тот статус, в котором был до отмены —
// @Description он сохранён в payload события project_cancelled. Если такого
// @Description события нет (отмена до появления ручки или правка статуса
// @Description руками), отвечаем 409: угаданный статус молча меняет, кого
// @Description проект ждёт.
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} restoreResp
// @Failure  404 {object} errorResponse "not_found"
// @Failure  409 {object} errorResponse "not_cancelled | no_cancel_event"
// @Router   /admin/projects/{id}/restore [post]
func (h *Handler) AdminRestoreProject(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	status, err := h.svc.RestoreProject(r.Context(), pid, actorID)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, restoreResp{Status: status})
}

// markTestReq — пометить проект тестовым или снять пометку.
type markTestReq struct {
	IsTest bool `json:"is_test"`
}

// AdminMarkProjectTest godoc
// @Summary  Пометить проект тестовым (или снять пометку)
// @Description Тестовые проекты по умолчанию скрыты из админских выдач
// @Description (include_test=true показывает их).
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string      true "project id"
// @Param    body body markTestReq true "is_test"
// @Success  204
// @Failure  404 {object} errorResponse "not_found"
// @Router   /admin/projects/{id}/mark_test [post]
func (h *Handler) AdminMarkProjectTest(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var in markTestReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	if err := h.svc.MarkProjectTest(r.Context(), pid, in.IsTest, actorID); err != nil {
		writeManagerErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MarkProjectTest — пометка «проект заведён для проверки».
func (s *Service) MarkProjectTest(ctx context.Context, projectID uuid.UUID, isTest bool, actorID uuid.UUID) error {
	return s.repo.MarkProjectTest(ctx, projectID, isTest, actorID)
}

// MarkProjectTest — выставить projects.is_test. Идемпотентно.
//
// В журнал пишем и «снял пометку»: тестовый проект не видно в списках, и
// пометка на настоящем проекте прячет живую работу — такое должно быть
// видно в истории.
func (r *Repo) MarkProjectTest(ctx context.Context, projectID uuid.UUID, isTest bool, actorID uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE projects SET is_test = $2, updated_at = now() WHERE id = $1`,
		projectID, isTest)
	if err != nil {
		return fmt.Errorf("mark test: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := audit.Write(ctx, tx, actorID, audit.ActionProjectMarkTest,
		audit.ObjectProject, projectID.String(),
		map[string]any{"is_test": isTest}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
