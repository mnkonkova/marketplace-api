package eventroute

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"marketpclce/internal/notifications"
	"marketpclce/internal/outbox"
	"marketpclce/internal/telegram"
)

// Доставка личных уведомлений в телеграм-боты.
//
// До этого «личные» события (CRMOnly в chat.go) честно считались,
// дедуплицировались в notification_log, уезжали в n8n — и попадали в
// ветку default, потому что workflow умеет писать только в общий чат
// менеджеров. Ни креатор, ни заказчик не получали НИЧЕГО: механизм
// доставки существовал только в комментариях.
//
// Ботов два, и адресат у события ровно один: напоминание о выкладке —
// разговор с креатором, «вышел новый ролик» — с заказчиком. Таблица
// ниже отвечает на один вопрос — «в какой бот», — и от вида проекта не
// зависит: там, где креатора нет по определению (brand_turnkey),
// поштучные напоминания и не эмитятся вовсе, а пинги такого проекта
// уходят в общий чат менеджеров.

// botRoute — куда и кому.
type botRoute struct {
	// bot — telegram.BotCreator или telegram.BotClient.
	bot string
	// keys — поля payload, из которых берутся получатели. Порядок
	// важен: первое непустое и выигрывает. Нескольких ключей хватает,
	// потому что события писались в разное время и называют человека
	// по-разному — «creator_user_id» у выкладок, «creator_id» у
	// заявок, «creator_ids» у задания проекта.
	keys []string
}

// botRouting — событие → бот. Тип, которого здесь нет, в бот не едет:
// молчание безопаснее догадки, а «кому это адресовано» — решение, а не
// свойство данных.
var botRouting = map[string]botRoute{
	// ---- разговор с КРЕАТОРОМ ----
	// Сроки выкладки: завтра, сегодня, вышло не везде, ручное
	// напоминание менеджера. Просрочка — в общий чат (это уже срыв), и
	// ЕЁ здесь намеренно нет.
	"project.publication_due_tomorrow": {telegram.BotCreator, []string{"creator_user_id"}},
	"project.publication_due_today":    {telegram.BotCreator, []string{"creator_user_id"}},
	"project.publication_incomplete":   {telegram.BotCreator, []string{"creator_user_id"}},
	"project.publication_manual":       {telegram.BotCreator, []string{"creator_user_id"}},
	// Проверка ролика: приняли или вернули с замечанием. Адресно, и
	// вернуть работу молча — худшее, что можно сделать.
	"project.publication_returned": {telegram.BotCreator, []string{"creator_user_id"}},
	"project.publication_accepted": {telegram.BotCreator, []string{"creator_user_id"}},
	// План подвинули: дату перенесли или день сняли. Сделал это
	// менеджер, а работать по ней креатору.
	"project.publication_moved":     {telegram.BotCreator, []string{"creator_user_id"}},
	"project.publication_cancelled": {telegram.BotCreator, []string{"creator_user_id"}},
	// Задание изменилось: материалы, чеклист, «вас добавили в проект»
	// (вместе с договором и ТЗ).
	"project.project_materials_updated": {telegram.BotCreator, []string{"creator_ids"}},
	"project.project_checklist_updated": {telegram.BotCreator, []string{"creator_ids"}},
	"project.project_creator_briefed":   {telegram.BotCreator, []string{"creator_ids"}},
	// Заявка «под ключ»: именное приглашение и рассылка всем
	// известным креаторам.
	"order.invitation_sent": {telegram.BotCreator, []string{"creator_id"}},
	"order.broadcast_sent":  {telegram.BotCreator, []string{"recipient_ids"}},

	// ---- разговор с ЗАКАЗЧИКОМ ----
	// Его собственные выключатели: порог просмотров, новый ролик,
	// сдвиг даты, недельная сводка. В чате менеджеров это был бы
	// пересказ того, что человек и так видит у себя.
	"project.client_views_threshold": {telegram.BotClient, []string{"client_user_id"}},
	"project.client_new_video":       {telegram.BotClient, []string{"client_user_id"}},
	"project.client_date_shift":      {telegram.BotClient, []string{"client_user_id"}},
	"project.client_weekly_digest":   {telegram.BotClient, []string{"client_user_id"}},
}

// BotRoutes — типы, которые едут в ботов. Для охранного теста: новый
// «личный» тип обязан либо попасть сюда, либо быть объявленным
// неличным осознанно.
func BotRoutes() map[string]string {
	out := make(map[string]string, len(botRouting))
	for k, v := range botRouting {
		out[k] = v.bot
	}
	return out
}

// BotResolver — кто превращает людей в чаты. Реализуется
// telegram.Service: он же держит дневной потолок и знает про
// заблокированных.
type BotResolver interface {
	Recipients(ctx context.Context, bot string, userIDs []uuid.UUID) ([]telegram.Recipient, error)
}

// Кому адресовано сообщение.
const (
	// audiencePerson — личное: креатору или заказчику, по привязке.
	audiencePerson = "person"
	// audienceManagers — в общий чат менеджеров.
	//
	// Раньше туда писал n8n через CRM-вебхук и телеграм-прокси. Теперь
	// тот же бот, что говорит с креаторами (решение владельца от 27
	// сентября): одна очередь доставки вместо двух, одно место, где
	// смотреть логи, и менеджеру не нужен отдельный бот ради доступа в
	// мини-апп — он заходит креаторским.
	audienceManagers = "managers"
)

// botEnvelope — что уезжает сервису бота: обычный payload плюс
// адресат.
//
// Получателей считаем МЫ, а не бот: у него нет ни привязок, ни
// дневного потолка, ни знания о том, кого этот проект касается.
// Отдать ему «событие, разберись сам» значит завести вторую копию
// правил доставки и однажды написать человеку дважды.
//
// У сообщения менеджерам список получателей пуст, и это не ошибка:
// чат один и известен самому боту — идентификатор группы живёт в его
// настройках. Нам о нём знать незачем: мы решаем ЧТО отправить и кому
// по смыслу, а «в какой чат» — вопрос доставки.
type botEnvelope struct {
	notifications.Payload
	Bot        string               `json:"bot"`
	Audience   string               `json:"audience"`
	Recipients []telegram.Recipient `json:"recipients"`
}

// botFanout — отправить событие в бот, если оно кому-то адресовано.
//
// Возвращает ошибку, по которой outbox повторит попытку: «письмо не
// ушло» надо чинить, а не забывать. Дедупликация на стороне бота — по
// event_id, поэтому повтор безопасен.
//
// Ноль адресатов — не ошибка и не повод писать в лог тревогу: человек
// мог не подключить бота вовсе. Это нормальное состояние продукта, где
// бот — дополнение к кабинету, а не единственный способ узнать.
func (d Deps) botFanout(
	ctx context.Context, outboxID int64, aggregate, aggregateID, eventType string, payload []byte,
) error {
	if d.Bot == nil {
		return nil
	}
	// Сообщения менеджерам. Отбирать некого: чат один, и он у бота в
	// настройках. Отправляем всё, что помечено «идёт в чат», — тем же
	// решением, которое принято в chat.go, а не вторым списком рядом.
	if delivery, known := DeliveryOf(eventType); known && delivery == ToChat {
		env := botEnvelope{
			Payload:  webhookPayload(outboxID, aggregate, aggregateID, eventType, payload),
			Bot:      telegram.BotCreator,
			Audience: audienceManagers,
		}
		env.AppBaseURL = d.AppBaseURL
		if err := d.Bot.SendEnvelope(ctx, env); err != nil {
			return fmt.Errorf("bot dispatch (managers): %w", err)
		}
		d.logger().Info("bot delivery", "event", eventType, "audience", audienceManagers)
		return nil
	}

	route, ok := botRouting[eventType]
	if !ok || d.BotUsers == nil {
		return nil
	}
	ids, err := recipientIDs(payload, route.keys)
	if err != nil {
		// Битый payload — это наша ошибка формата, а не сбой связи:
		// ретраи её не вылечат.
		return fmt.Errorf("%w: bot recipients: %v", outbox.ErrPermanent, err)
	}
	if len(ids) == 0 {
		return nil
	}
	recipients, err := d.BotUsers.Recipients(ctx, route.bot, ids)
	if err != nil {
		return fmt.Errorf("bot recipients: %w", err)
	}
	if len(recipients) == 0 {
		d.logger().Debug("bot delivery: никто не подключён",
			"event", eventType, "bot", route.bot, "candidates", len(ids))
		return nil
	}
	env := botEnvelope{
		Payload: webhookPayload(outboxID, aggregate, aggregateID, eventType, payload),
		// Адрес кабинета: ссылка в сообщении ведёт туда, где человек
		// сделает то, о чём его просят.
		Bot:        route.bot,
		Audience:   audiencePerson,
		Recipients: recipients,
	}
	env.AppBaseURL = d.AppBaseURL
	if err := d.Bot.SendEnvelope(ctx, env); err != nil {
		return fmt.Errorf("bot dispatch: %w", err)
	}
	d.logger().Info("bot delivery",
		"event", eventType, "bot", route.bot, "recipients", len(recipients))
	return nil
}

// recipientIDs — вытащить людей из payload по известным ключам.
//
// Первый непустой ключ и выигрывает: события писались в разное время и
// называют человека по-разному, но в одном событии он назван один раз.
func recipientIDs(payload []byte, keys []string) ([]uuid.UUID, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	for _, k := range keys {
		v, ok := raw[k]
		if !ok || string(v) == "null" {
			continue
		}
		// Одиночный id и список — оба валидны: «вас добавили в проект»
		// приходит списком, «ваш ролик приняли» — одним человеком.
		var one uuid.UUID
		if err := json.Unmarshal(v, &one); err == nil {
			if one == uuid.Nil {
				continue
			}
			return []uuid.UUID{one}, nil
		}
		var many []uuid.UUID
		if err := json.Unmarshal(v, &many); err == nil && len(many) > 0 {
			return many, nil
		}
	}
	return nil, nil
}
