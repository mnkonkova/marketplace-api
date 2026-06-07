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
)

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
