package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// Пакетная передача проектов другому менеджеру.
//
// Нужна там же, где 409 на снятии роли: сотрудник уходит, и его двадцать
// проектов надо передать до того, как у него заберут доступ. Двадцать
// одиночных вызовов /assign — это двадцать шансов остановиться на
// середине и получить половину проектов у одного, половину у другого.
// Поэтому одна транзакция: либо переезжают все, либо никто.
//
// События при этом такие же, как у одиночного назначения, и на каждый
// проект своё: n8n рассылает уведомления по проектам, и одно событие на
// пачку означало бы, что девятнадцать участников ничего не узнали.

// ErrSameManager — передать проекты самому себе. Не ошибка данных, но и
// не действие: почти всегда это промах в выпадающем списке.
var ErrSameManager = errors.New("target manager is the same as source")

// ErrNotManagerTarget — принимающий не менеджер (или не одобрен). Отдать
// проекты человеку без прав — тот же «проект без ответственного», только
// с заполненной колонкой.
var ErrNotManagerTarget = errors.New("target user is not an approved manager")

// TransferProjects — передать незавершённые проекты менеджера from
// менеджеру to. ids пустой — передаём все незавершённые.
//
// Возвращает id переданных проектов в том порядке, в каком они лежали в
// списке менеджера.
func (r *Repo) TransferProjects(ctx context.Context, from, to uuid.UUID, ids []uuid.UUID, actorID uuid.UUID) ([]uuid.UUID, error) {
	if from == to {
		return nil, ErrSameManager
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT TRUE FROM users
		 WHERE id = $1 AND is_active = TRUE AND is_approved = TRUE
		   AND (is_manager = TRUE OR is_admin = TRUE)`, to).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotManagerTarget
		}
		return nil, fmt.Errorf("check target: %w", err)
	}

	// Блокируем строки: между выбором проектов и их переназначением
	// кто-то может успеть назначить менеджеру ещё один — и он остался бы
	// у уходящего сотрудника.
	q := `
SELECT id FROM projects
WHERE assigned_to_user_id = $1 AND status IN ('draft','active','on_hold','dispute')`
	args := []any{from}
	if len(ids) > 0 {
		args = append(args, ids)
		q += fmt.Sprintf(" AND id = ANY($%d)", len(args))
	}
	q += " ORDER BY updated_at DESC FOR UPDATE"

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select projects to transfer: %w", err)
	}
	moved := make([]uuid.UUID, 0, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan project id: %w", err)
		}
		moved = append(moved, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, id := range moved {
		target := to
		if err := assignManagerInTx(ctx, tx, id, &target, actorID); err != nil {
			return nil, err
		}
	}
	// Отдельная запись на всю операцию: по журналу должно читаться «сдал
	// дела такому-то», а не только двадцать одинаковых назначений.
	if err := audit.Write(ctx, tx, actorID, audit.ActionProjectTransferBatch,
		audit.ObjectUser, from.String(), map[string]any{
			"to_user_id": to.String(),
			"projects":   len(moved),
		}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return moved, nil
}

// TransferProjects — передача проектов между менеджерами (админская).
func (s *Service) TransferProjects(ctx context.Context, from, to uuid.UUID, ids []uuid.UUID, actorID uuid.UUID) ([]uuid.UUID, error) {
	return s.repo.TransferProjects(ctx, from, to, ids, actorID)
}

type transferReq struct {
	// ToUserID — кому передаём. Обязателен.
	ToUserID string `json:"to_user_id"`
	// ProjectIDs — что передаём. Пусто = все незавершённые проекты
	// менеджера: это основной сценарий (сотрудник уходит).
	ProjectIDs []string `json:"project_ids,omitempty"`
}

type transferResp struct {
	// Transferred — сколько проектов переехало.
	Transferred int         `json:"transferred"`
	ProjectIDs  []uuid.UUID `json:"project_ids"`
}

// AdminTransferProjects godoc
// @Summary  Передать проекты менеджера другому менеджеру
// @Description Одна транзакция: либо переезжают все проекты, либо никто.
// @Description На каждый проект — своё событие и запись в outbox, как при
// @Description одиночном назначении. project_ids пусто = все незавершённые.
// @Tags     admin-users
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string      true "user id менеджера, с которого снимаем"
// @Param    body body transferReq true "to_user_id + опционально project_ids"
// @Success  200 {object} transferResp
// @Failure  400 {object} errorResponse "bad_user_id | invalid_input — передача самому себе или принимающий не менеджер"
// @Router   /admin/managers/{id}/transfer_projects [post]
func (h *Handler) AdminTransferProjects(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	from, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id менеджера в URL.")
		return
	}
	var in transferReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	to, err := uuid.Parse(strings.TrimSpace(in.ToUserID))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_user_id",
			"to_user_id обязателен и должен быть UUID.")
		return
	}
	ids := make([]uuid.UUID, 0, len(in.ProjectIDs))
	for _, raw := range in.ProjectIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_project_id",
				"project_ids должен содержать UUID проектов.")
			return
		}
		ids = append(ids, id)
	}
	moved, err := h.svc.TransferProjects(r.Context(), from, to, ids, actorID)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, transferResp{Transferred: len(moved), ProjectIDs: moved})
}
