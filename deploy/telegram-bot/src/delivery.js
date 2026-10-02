// Рассылка одного события адресатам — с памятью «кому уже ушло».
//
// Вынесено из server.js, чтобы проверять скриптом без Telegram.
//
// Зачем память по адресату, а не по событию. Событие — это несколько
// получателей. Раньше событие помечалось «сделано» ДО отправки, целиком:
//   • упала отправка второму адресату — сообщение очереди вернулось с
//     ошибкой, но повтор тут же отбрасывался как дубль, и второй
//     адресат не получал ничего;
//   • message_id первого, уже доставленного, пропадали вместе с
//     исключением — API не узнавал, к какому проекту ответ на него.
//
// Теперь так:
//   • помним по событию, в какие чаты ушло и под каким message_id
//     (0 — ушло без номера или человек заблокировал бота: слать
//     повторно не надо, запоминать нечего);
//   • отправка одному упала — остальным всё равно шлём (они не
//     виноваты), а в конце бросаем PartialDeliveryError, в которой
//     лежат message_id всех успешных. Вызывающий квитирует сообщение
//     как failed (очередь повторит) и всё равно сообщает API sent — API
//     запоминает соответствия и для failed-строк;
//   • на повторе тем, кому уже ушло, не шлём, но их message_id снова
//     отдаём в messages: если прошлая квитанция потерялась, API узнает
//     их сейчас; если дошла — вставка у API идемпотентна (ON CONFLICT
//     DO NOTHING). Шлём только тем, кому не ушло.
//
// Память процессная: перезапуск контейнера её теряет, и повтор после
// него дойдёт до всех ещё раз — та же цена, что и раньше.

export class PartialDeliveryError extends Error {
  constructor(cause, messages) {
    super(String(cause?.message || cause));
    this.name = 'PartialDeliveryError';
    this.cause = cause;
    // [{ chat_id, message_id }] — что всё-таки ушло.
    this.messages = messages;
  }
}

/**
 * Память доставленного: eventID → Map(chatKey → message_id).
 * Ограничена по времени и размеру: окно повторов — минуты.
 */
export function createDeliveryMemory({ ttlMs = 6 * 60 * 60 * 1000, max = 5000, now = () => Date.now() } = {}) {
  const seen = new Map();
  return {
    /**
     * Чаты, куда это событие уже ушло. Без eventID дедуп невозможен —
     * отдаём пустую одноразовую запись.
     */
    done(eventID) {
      if (!eventID) return new Map();
      const t = now();
      for (const [k, e] of seen) {
        if (t - e.at > ttlMs) seen.delete(k);
      }
      let e = seen.get(eventID);
      if (!e) {
        if (seen.size >= max) seen.delete(seen.keys().next().value);
        e = { at: t, chats: new Map() };
        seen.set(eventID, e);
      }
      return e.chats;
    },
  };
}

/**
 * Разослать личные сообщения события.
 *
 * send(r) → сообщение Telegram (с message_id) или null, если отправлять
 * нечего (нет текста). isBlocked(err) — человек заблокировал бота;
 * тогда onBlocked(r) и идём дальше.
 *
 * Возвращает { sent, blocked, skipped, messages }; при сбое хотя бы
 * одной отправки бросает PartialDeliveryError с messages успешных.
 */
export async function deliverToPeople({ recipients, done, send, isBlocked, onBlocked }) {
  let sent = 0;
  let blocked = 0;
  let skipped = 0;
  const messages = [];
  let firstErr = null;
  for (const r of recipients) {
    const chat = r.tg_chat_id;
    if (done.has(chat)) {
      skipped += 1;
      const id = done.get(chat);
      if (id) messages.push({ chat_id: chat, message_id: id });
      continue;
    }
    try {
      const msg = await send(r);
      const id = msg?.message_id || 0;
      done.set(chat, id);
      if (id) messages.push({ chat_id: chat, message_id: id });
      if (msg) sent += 1;
    } catch (err) {
      if (isBlocked(err)) {
        await Promise.resolve(onBlocked(r)).catch(() => {});
        done.set(chat, 0);
        blocked += 1;
        continue;
      }
      // Не помечаем — повтор пошлёт именно ему.
      firstErr = firstErr || err;
    }
  }
  if (firstErr) throw new PartialDeliveryError(firstErr, messages);
  return { sent, blocked, skipped, messages };
}
