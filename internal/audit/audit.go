// Package audit — журнал админских действий (таблица admin_audit_log).
//
// Пишется ТОЛЬКО из той же транзакции, что и само действие: журнал,
// написанный отдельным подключением, врёт при откате — роль не снялась,
// а в истории «снял роль» осталось. Поэтому Write принимает pgx.Tx, а не
// пул, и отдельного «запиши мне в журнал» из хендлера нет.
//
// Пакет намеренно ни от чего внутреннего не зависит: в него ходят и
// projects, и billing, и admin, а обратные связи в эти домены завели бы
// цикл импортов.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Типы объектов журнала. Объектов разных таблиц, поэтому пара
// (object_type, object_id), а не внешний ключ.
const (
	ObjectUser      = "user"
	ObjectProject   = "project"
	ObjectTerms     = "terms_version"
	ObjectChecklist = "checklist_template"
)

// Действия. Список закрытый: фронт по нему рисует человеческие названия,
// и свободная строка на каждой ручке быстро развалила бы фильтр журнала.
const (
	ActionUserPromoteManager = "user.promote_manager"
	ActionUserRevokeManager  = "user.revoke_manager"
	ActionUserApproveManager = "user.approve_manager"
	ActionUserDeactivate     = "user.deactivate"
	ActionUserActivate       = "user.activate"
	ActionUserVerifyEmail    = "user.verify_email"
	ActionUserMarkTest       = "user.mark_test"
	ActionUserLoginLink      = "user.login_link"

	ActionModerationApprove = "moderation.approve"
	ActionModerationReject  = "moderation.reject"

	ActionTermsPublish     = "terms.publish"
	ActionChecklistPublish = "checklist.publish"

	ActionProjectCancel        = "project.cancel"
	ActionProjectRestore       = "project.restore"
	ActionProjectAssignManager = "project.assign_manager"
	ActionProjectMarkTest      = "project.mark_test"
	ActionProjectTransferBatch = "project.transfer_batch"
)

// Entry — одна запись журнала.
type Entry struct {
	ID int64 `json:"id"`
	// ActorUserID — кто совершил действие. Nil означает «не человек»
	// (фоновая задача) либо вызов из теста мимо HTTP: такие записи
	// ложатся с NULL, а не с несуществующим пользователем.
	ActorUserID *uuid.UUID `json:"actor_user_id,omitempty"`
	// ActorEmail/ActorDisplayName — заполняются только на чтении (JOIN
	// на users). На записи их нет: почта сотрудника может смениться, а
	// журнал должен показывать сегодняшнюю.
	ActorEmail       string         `json:"actor_email,omitempty"`
	ActorDisplayName string         `json:"actor_display_name,omitempty"`
	Action           string         `json:"action"`
	ObjectType       string         `json:"object_type"`
	ObjectID         string         `json:"object_id,omitempty"`
	Payload          map[string]any `json:"payload,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
}

// Write — запись в журнал в транзакции действия.
//
// actor == uuid.Nil кладётся как NULL: внешний ключ на users не пустит
// нулевой UUID, а терять само действие из-за отсутствия актора нельзя.
func Write(ctx context.Context, tx pgx.Tx, actor uuid.UUID, action, objectType, objectID string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audit payload: %w", err)
	}
	var actorArg any
	if actor != uuid.Nil {
		actorArg = actor
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO admin_audit_log (actor_user_id, action, object_type, object_id, payload)
VALUES ($1, $2, $3, $4, $5)`, actorArg, action, objectType, objectID, data); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}
