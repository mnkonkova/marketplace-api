package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	AggregateSpecialist = "specialist"
	AggregateEmail      = "email"
	// CRM v5: события проектов (создан, шаг изменился, стадия продвинута,
	// взят менеджером, в диспуте, завершён, нужны правки). Воркер диспатчит
	// их в n8n webhook (см. cmd/worker/main.go).
	AggregateProject = "project"
	// Support — обращения в поддержку из футера UI. Один тип события
	// (message_received), воркер шлёт в n8n который дампит в Telegram.
	AggregateSupport = "support"
	// Portfolio — события portfolio_items. Сейчас один event:
	// video_uploaded → воркер транскодит превью (см. docs/VIDEO_TRANSCODING.md).
	AggregatePortfolio = "portfolio"
	// Moderation — события модерации публикаций спецов админом.
	// Сейчас один event: specialist_pending → воркер уведомляет n8n,
	// чтобы админу пришла нотификация. См. docs/SPECIALIST_MODERATION.md.
	AggregateModeration = "moderation"

	EventSpecialistUpserted  = "specialist.upserted"
	EventSpecialistPublished = "specialist.published"
	EventSpecialistRetracted = "specialist.retracted"
	EventSpecialistDeleted   = "specialist.deleted"

	// EventEmailVerifySend — payload: {to, to_name, token, base_url}.
	// Воркер на это событие шлёт письмо подтверждения в n8n webhook (workflow crmEmailNotify).
	EventEmailVerifySend = "email.verify_send"

	// EventEmailPasswordResetSend — payload: {to, to_name, token, base_url}.
	// Письмо со ссылкой на сброс пароля (BaseURL + /auth/reset?token=).
	EventEmailPasswordResetSend = "email.password_reset_send"

	// CRM v5: project.* — пробрасываются как есть в n8n. n8n сам решает
	// что слать (email/telegram/...) по event_type'у.
	EventProjectCreated          = "project.created"
	EventProjectStepTransitioned = "project.step_transitioned"
	EventProjectStageAdvanced    = "project.stage_advanced"
	EventProjectAssigned         = "project.assigned"
	EventProjectDisputed         = "project.disputed"
	EventProjectCompleted        = "project.completed"
	EventProjectCommentAdded     = "project.comment_added"

	EventSupportMessageReceived = "support.message_received"

	// EventPortfolioVideoUploaded — спец залил новое видео (multipart-upload
	// завершён + DB-запись создана). Воркер по этому событию качает
	// оригинал из S3, гонит через ffmpeg (480p H.264, 5-10 сек) и пишет
	// preview_url в portfolio_items. payload — PortfolioVideoUploadedPayload.
	EventPortfolioVideoUploaded = "portfolio.video_uploaded"

	// EventModerationSpecialistPending — спец встал в очередь модерации
	// (запросил публикацию или поменял профиль будучи approved). payload —
	// ModerationSpecialistPendingPayload. Воркер дёргает n8n, чтобы админ
	// получил уведомление (telegram/email/...) и пошёл в кабинет одобрять.
	EventModerationSpecialistPending = "moderation.specialist_pending"
)

// ModerationSpecialistPendingPayload — payload для admin-уведомления:
// что именно встало в очередь, чтобы n8n мог сразу собрать ссылку
// {base_url}/admin/moderation/{user_id}.
type ModerationSpecialistPendingPayload struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	// Reason: "publish_requested" — спец нажал «Опубликовать»;
	//         "content_changed"   — approved-спец отредактировал профиль.
	Reason  string `json:"reason"`
	BaseURL string `json:"base_url,omitempty"`
}

// EmailVerifyPayload — структура payload для EventEmailVerifySend.
// Объявлено в outbox, чтобы и emitter (auth.Service) и handler (cmd/worker)
// зависели от одной формы.
type EmailVerifyPayload struct {
	To      string `json:"to"`
	ToName  string `json:"to_name,omitempty"`
	Token   string `json:"token"`    // raw токен (нешифрованный) — для вставки в URL
	BaseURL string `json:"base_url"` // публичный URL фронта (APP_BASE_URL)
}

// EmailPasswordResetPayload — payload для EventEmailPasswordResetSend.
// Та же форма что у EmailVerifyPayload, но отдельный тип чтобы хендлер
// в воркере не путал шаблоны и не отправил «подтвердите почту» вместо
// «сброс пароля».
type EmailPasswordResetPayload struct {
	To      string `json:"to"`
	ToName  string `json:"to_name,omitempty"`
	Token   string `json:"token"`
	BaseURL string `json:"base_url"`
}

// PortfolioVideoUploadedPayload — данные для transcoding-пайплайна.
// ItemID — uuid записи в portfolio_items (handler обновит preview_*
// колонки в ней). S3Key — относительный ключ оригинала
// (portfolio/<user>/<item>.mp4), preview уйдёт рядом как
// portfolio/<user>/<item>_preview.mp4.
type PortfolioVideoUploadedPayload struct {
	ItemID string `json:"item_id"`
	UserID string `json:"user_id"`
	S3Key  string `json:"s3_key"`
}

func Emit(ctx context.Context, tx pgx.Tx, aggregate, aggregateID, eventType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	const q = `
INSERT INTO outbox (aggregate, aggregate_id, event_type, payload)
VALUES ($1, $2, $3, $4)`
	if _, err := tx.Exec(ctx, q, aggregate, aggregateID, eventType, data); err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}
	return nil
}

// Проектная страница (00032). Как и остальные project.* — уходят в n8n
// как есть, он сам решает, что слать креатору, менеджеру и клиенту.
const (
	// EventPublicationsCreated — менеджер проставил даты пачкой.
	// payload: {project_id, batch_id, count, created_by}.
	EventPublicationsCreated = "project.publications_created"

	// EventPublicationSubmitted — креатор сдал ссылки. payload содержит
	// status ("partial"|"done") и список сданных площадок: по нему n8n
	// отличает «вышло полностью» от «вышло, но не везде».
	EventPublicationSubmitted = "project.publication_submitted"

	// EventPublicationClosed — менеджер закрыл неполную выкладку руками.
	EventPublicationClosed = "project.publication_closed"

	// EventPublicationLinkEdited — менеджер исправил сданную ссылку.
	// payload: {project_id, publication_id, platform, url, stats_reset}.
	// Отдельное событие, а не publication_submitted: сдал креатор, а
	// правил менеджер, и в истории проекта это разные строки.
	EventPublicationLinkEdited = "project.publication_link_edited"

	// EventPublicationReturned — менеджер вернул ролик креатору с
	// замечанием. payload: {project_id, publication_id, comment,
	// failed_items, decided_by}.
	EventPublicationReturned = "project.publication_returned"

	// EventPublicationAccepted — ролик принят. Тот же payload;
	// failed_items у принятого пуст по определению.
	EventPublicationAccepted = "project.publication_accepted"

	// EventProjectPeriodClosed — период проекта подытожен: просмотры
	// заморожены срезом, суммы посчитаны в последний раз. payload:
	// {project_id, title, period_seq, starts_on, ends_on, videos, views,
	// total, snapshot_as_of, snapshot_approx}. Уходит в общий чат:
	// это единственный момент, когда по периоду становится что
	// обсуждать, и происходит он сам, без человека.
	EventProjectPeriodClosed = "project.period_closed"
)

// Самостоятельный подбор креаторов клиентом (00033). Как и остальные
// project.* — уходят в n8n как есть.
const (
	// EventOrderInvitationSent — приглашение ушло креатору. payload:
	// {order_id, creator_id, priority, expires_at}.
	EventOrderInvitationSent = "order.invitation_sent"

	EventOrderInvitationAccepted = "order.invitation_accepted"
	EventOrderInvitationDeclined = "order.invitation_declined"

	// EventOrderInvitationExpired — креатор не ответил, приглашение
	// сгорело и место освободилось.
	EventOrderInvitationExpired = "order.invitation_expired"

	// EventOrderCandidateSilent — сутки молчания. Уходит МЕНЕДЖЕРУ:
	// самому креатору второй раз не пишем, его уже позвали.
	EventOrderCandidateSilent = "order.candidate_silent"

	// EventOrderStaffed — согласилось столько, сколько нужно.
	EventOrderStaffed = "order.staffed"

	// EventOrderNeedMore — резерв кончился, а состав не собран.
	// Дальше без менеджера не обойтись.
	EventOrderNeedMore = "order.need_more"
)
