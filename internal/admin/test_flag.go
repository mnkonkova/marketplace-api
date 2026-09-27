package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// Пометка «пользователь заведён для проверки» — парная projects.is_test.
//
// Считать её по данным нельзя: тестовый аккаунт заводят на настоящую
// почту, а «test» в имени пишут не всегда. Ставит тот, кто знает.

// MarkUserTest — выставить users.is_test. Идемпотентно.
//
// Админа пометить тестовым нельзя: он пропал бы из списка команды, и
// первым делом — из списка тех, у кого есть право это исправить.
func (r *Repo) MarkUserTest(ctx context.Context, userID uuid.UUID, isTest bool, actorID uuid.UUID) error {
	return r.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE users SET is_test = $2, updated_at = now()
			 WHERE id = $1 AND is_admin = FALSE`,
			userID, isTest)
		if err != nil {
			return fmt.Errorf("mark user test: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var isAdmin bool
			if perr := tx.QueryRow(ctx,
				`SELECT is_admin FROM users WHERE id = $1`, userID).Scan(&isAdmin); perr != nil {
				return ErrNotFound
			}
			if isAdmin {
				return fmt.Errorf("%w: админа нельзя пометить тестовым", ErrInvalidInputRepo)
			}
			return ErrNotFound
		}
		return audit.Write(ctx, tx, actorID, audit.ActionUserMarkTest,
			audit.ObjectUser, userID.String(), map[string]any{"is_test": isTest})
	})
}

// MarkUserTest — пометить пользователя тестовым (или снять пометку).
func (s *Service) MarkUserTest(ctx context.Context, userID uuid.UUID, isTest bool, actorID uuid.UUID) error {
	err := s.repo.MarkUserTest(ctx, userID, isTest, actorID)
	if errors.Is(err, ErrInvalidInputRepo) {
		return fmt.Errorf("%w: админа нельзя пометить тестовым", ErrInvalidInput)
	}
	return err
}

type markUserTestReq struct {
	IsTest bool `json:"is_test"`
}

// AdminMarkUserTest godoc
// @Summary  Пометить пользователя тестовым (или снять пометку)
// @Description Тестовые пользователи по умолчанию скрыты из админских
// @Description выдач (include_test=true показывает их). Админа пометить
// @Description нельзя — он пропал бы из списка команды.
// @Tags     admin-users
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string          true "user id"
// @Param    body body markUserTestReq true "is_test"
// @Success  204
// @Failure  400 {object} errorResponse "invalid_input — попытка пометить админа"
// @Failure  404 {object} errorResponse "not_found"
// @Router   /admin/users/{id}/mark_test [post]
func (h *Handler) AdminMarkUserTest(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	actor, _ := auth.UserIDFrom(r.Context())
	var in markUserTestReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	if err := h.svc.MarkUserTest(r.Context(), id, in.IsTest, actor); err != nil {
		writeServiceErr(w, err)
		return
	}
	auditLog(r, "user.mark_test", id, "is_test", in.IsTest)
	w.WriteHeader(http.StatusNoContent)
}
