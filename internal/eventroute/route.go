// Package eventroute — маршрутизация outbox-событий по обработчикам.
//
// Раньше это жило анонимными замыканиями внутри main() воркера: какой
// агрегат в какой из трёх вебхуков уходит, решалось в семидесяти строках
// посреди инициализации, и позвать их из теста было нельзя в принципе.
// Перепутать местами адреса — CRM-события в почтовый вебхук — никто бы
// не заметил: воркер считает доставку успешной по коду 200.
//
// Здесь те же обработчики, но обычными функциями и с явной таблицей
// «агрегат → обработчик», которую можно проверить тестом.
package eventroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/notifications"
	"marketpclce/internal/outbox"
	"marketpclce/internal/transcode"
)

// Dispatcher — отправка события во внешнюю систему (n8n). Интерфейс, а
// не *notifications.WebhookDispatcher, чтобы тест мог подставить свой и
// проверить, что именно уехало и куда.
//
// nil означает «адрес не задан»: событие тихо квитируется, чтобы не
// копить ретраи на стенде без вебхуков.
type Dispatcher interface {
	Send(ctx context.Context, p notifications.Payload) error
}

// SpecialistIndexer — часть search.Indexer, которой пользуется
// обработчик specialist.*.
type SpecialistIndexer interface {
	Reconcile(ctx context.Context, userID uuid.UUID, versionMicro int64) error
	Delete(ctx context.Context, userID uuid.UUID, versionMicro int64) error
}

// FeedIndexer — часть feed-индексатора: видео специалиста в ленте.
type FeedIndexer interface {
	ReconcileVideos(ctx context.Context, userID uuid.UUID) error
	DeleteByUser(ctx context.Context, userID uuid.UUID) error
}

// Transcoder — обработка залитого видео (ffmpeg). nil = транскодинг
// выключен, событие квитируется как no-op.
type Transcoder interface {
	Process(ctx context.Context, payload []byte) error
}

// Deps — уже собранные зависимости. Их конструирование (ffmpeg, S3,
// адреса вебхуков) остаётся в main: там же, где os.Exit при поломке.
type Deps struct {
	// CRM — общий вебхук CRM-событий: project.* и moderation.*.
	CRM Dispatcher
	// Email — отдельный вебхук писем (verify, сброс пароля).
	Email Dispatcher
	// Support — отдельный вебхук обращений в поддержку.
	Support Dispatcher

	Search SpecialistIndexer
	Feed   FeedIndexer
	// Transcoder — nil, если ffmpeg или S3 не настроены.
	Transcoder Transcoder

	Logger *slog.Logger
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Handlers — таблица «агрегат → обработчик» для outbox-воркера.
//
// Агрегат без обработчика воркер молча помечает обработанным (см.
// outbox.Worker: «outbox no handler» в warn и success-эквивалент), то
// есть событие теряется без единой ошибки. Поэтому таблица собирается
// в одном месте и накрыта тестом, который падает, если в outbox
// завели агрегат, а сюда его не добавили.
func Handlers(d Deps) map[string]outbox.Handler {
	return map[string]outbox.Handler{
		outbox.AggregateSpecialist: d.SpecialistHandler,
		outbox.AggregateEmail:      d.EmailHandler,
		outbox.AggregateProject:    d.CRMHandler(outbox.AggregateProject),
		outbox.AggregateModeration: d.CRMHandler(outbox.AggregateModeration),
		outbox.AggregateSupport:    d.SupportHandler,
		outbox.AggregatePortfolio:  d.PortfolioHandler,
	}
}

// CRMHandler — project.* и moderation.* уходят в один и тот же вебхук
// CRM: n8n ветвится по event_type и решает, что писать в чат. Агрегат
// передаётся параметром, потому что в теле запроса он свой у каждого, а
// адрес общий.
func (d Deps) CRMHandler(aggregate string) outbox.Handler {
	return func(ctx context.Context, outboxID int64, aggregateID, eventType string, payload []byte) error {
		if d.CRM == nil {
			// Адрес не задан (локальный запуск) — квитируем, чтобы не
			// копить ретраи.
			return nil
		}
		return d.CRM.Send(ctx, webhookPayload(outboxID, aggregate, aggregateID, eventType, payload))
	}
}

// SupportHandler — обращения в поддержку идут своим вебхуком: у них
// отдельный workflow с дампом в Telegram и письмом на info@.
func (d Deps) SupportHandler(ctx context.Context, outboxID int64, aggregateID, eventType string, payload []byte) error {
	if d.Support == nil {
		return nil
	}
	return d.Support.Send(ctx, webhookPayload(outboxID, outbox.AggregateSupport, aggregateID, eventType, payload))
}

// EmailHandler — письма (подтверждение почты, сброс пароля) уходят в
// свой вебхук: n8n рендерит HTML и отдаёт провайдеру.
//
// Без адреса ссылку печатаем в stdout и квитируем: локальный запуск без
// n8n должен оставаться рабочим, иначе никто не подтвердит почту.
func (d Deps) EmailHandler(ctx context.Context, outboxID int64, aggregateID, eventType string, payload []byte) error {
	if d.Email != nil {
		return d.Email.Send(ctx, webhookPayload(outboxID, "user", aggregateID, eventType, payload))
	}
	switch eventType {
	case outbox.EventEmailVerifySend:
		var p outbox.EmailVerifyPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("decode email payload: %w", err)
		}
		d.logger().Info("verify-email (n8n disabled, copy this URL manually)",
			"to", p.To, "url", p.BaseURL+"/verify?token="+p.Token)
		return nil
	case outbox.EventEmailPasswordResetSend:
		var p outbox.EmailPasswordResetPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("decode password reset payload: %w", err)
		}
		d.logger().Info("password-reset (n8n disabled, copy this URL manually)",
			"to", p.To, "url", p.BaseURL+"/auth/reset?token="+p.Token)
		return nil
	default:
		d.logger().Warn("unknown email event", "type", eventType)
		return nil
	}
}

// SpecialistHandler — переиндексация специалиста в поиске и в ленте.
func (d Deps) SpecialistHandler(ctx context.Context, _ int64, aggregateID, eventType string, payload []byte) error {
	uid, err := uuid.Parse(aggregateID)
	if err != nil {
		return err
	}
	// version_micro — внешняя версия (updated_at.UnixMicro() эмиттера)
	// для external_gte OCC в OpenSearch. Старые события без поля → 0,
	// индексатор фолбэчится на безверсионный путь.
	var p struct {
		VersionMicro int64 `json:"version_micro"`
	}
	_ = json.Unmarshal(payload, &p) // невалидный payload = version 0
	if eventType == outbox.EventSpecialistDeleted {
		if err := d.Search.Delete(ctx, uid, p.VersionMicro); err != nil {
			return err
		}
		return d.Feed.DeleteByUser(ctx, uid)
	}
	if err := d.Search.Reconcile(ctx, uid, p.VersionMicro); err != nil {
		return err
	}
	return d.Feed.ReconcileVideos(ctx, uid)
}

// PortfolioHandler — транскодинг превью залитого видео. Без ffmpeg или
// без S3 (Transcoder == nil) событие квитируется как no-op: воркер
// должен подниматься локально без них.
func (d Deps) PortfolioHandler(ctx context.Context, _ int64, aggregateID, eventType string, payload []byte) error {
	if d.Transcoder == nil {
		d.logger().Info("transcode no-op (handler disabled)",
			"aggregate_id", aggregateID, "event", eventType)
		return nil
	}
	if eventType != outbox.EventPortfolioVideoUploaded {
		return fmt.Errorf("%w: unknown portfolio event %q", outbox.ErrPermanent, eventType)
	}
	err := d.Transcoder.Process(ctx, payload)
	if err != nil && errors.Is(err, transcode.ErrPermanent) {
		return fmt.Errorf("%w: %v", outbox.ErrPermanent, err)
	}
	return err
}

// webhookPayload — тело запроса в n8n.
//
// EventID — id строки outbox, и это не косметика: n8n дедуплицирует по
// нему. Раньше id собирали как aggregate_id + тип события, и два
// одинаковых события по одному проекту были для n8n одним.
func webhookPayload(outboxID int64, aggregate, aggregateID, eventType string, payload []byte) notifications.Payload {
	return notifications.Payload{
		EventID:     strconv.FormatInt(outboxID, 10),
		Aggregate:   aggregate,
		AggregateID: aggregateID,
		EventType:   eventType,
		Data:        payload,
		OccurredAt:  time.Now().UTC(),
	}
}
