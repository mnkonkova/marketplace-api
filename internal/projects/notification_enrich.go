package projects

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// enrichProjectPayload — догружает в payload поля project-карточки
// (title, budget, client_name, client_contact, specialist_display_name),
// чтобы n8n мог собирать богатые нотификации без обратного HTTP-вызова
// в API. Вызывать перед emit() в той же транзакции; молча no-op при
// ошибке SELECT — payload остаётся минимальным, но событие не теряется.
//
// Для зарегистрированных клиентов (client_user_id != NULL) берём
// display_name/email из users; для no-account — из самой строки project.
//
// ⚠️ При изменении набора полей карточки (новая колонка в projects,
// новый источник имени клиента, и т.п.) — обновлять и SELECT здесь,
// и шаблон в n8n Code-node «CRM project events → Telegram».
// Альтернатива на будущее, если payload разрастётся: n8n берёт
// project_id из webhook и сам ходит в `GET /admin/projects/{id}` через
// service-token. Тогда этот helper удалится, а схему таблиц будет
// знать только API. См. также docs/VDS_SETUP.md §7 (n8n).
func enrichProjectPayload(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, payload map[string]any) {
	var (
		title         string
		budget        sql.NullInt64
		clientName    sql.NullString
		clientContact sql.NullString
		clientDisplay sql.NullString
		clientEmail   sql.NullString
		specDisplay   sql.NullString
	)
	// display_name живёт в *_profiles, не в users — джойним по двум таблицам.
	// users даём только email для контакта клиента.
	err := tx.QueryRow(ctx, `
SELECT p.title, p.budget, p.client_name, p.client_contact,
       cp.display_name, cu.email,
       sp.display_name
FROM projects p
LEFT JOIN users cu              ON cu.id = p.client_user_id
LEFT JOIN client_profiles cp    ON cp.user_id = p.client_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = p.specialist_user_id
WHERE p.id = $1`, projectID).Scan(
		&title, &budget, &clientName, &clientContact,
		&clientDisplay, &clientEmail, &specDisplay)
	if err != nil {
		return
	}
	payload["title"] = title
	if budget.Valid {
		payload["budget"] = budget.Int64
	}
	switch {
	case clientName.Valid && clientName.String != "":
		payload["client_name"] = clientName.String
	case clientDisplay.Valid && clientDisplay.String != "":
		payload["client_name"] = clientDisplay.String
	}
	switch {
	case clientContact.Valid && clientContact.String != "":
		payload["client_contact"] = clientContact.String
	case clientEmail.Valid && clientEmail.String != "":
		payload["client_contact"] = clientEmail.String
	}
	if specDisplay.Valid && specDisplay.String != "" {
		payload["specialist_display_name"] = specDisplay.String
	}
}
