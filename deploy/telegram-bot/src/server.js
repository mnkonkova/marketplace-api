// Сервис двух телеграм-ботов «PrMarket».
//
// Делает ровно две вещи:
//   • принимает апдейты Telegram (вебхук) — /start с кодом привязки,
//     блокировку бота, обычные сообщения;
//   • принимает уведомления от нашего API и рассылает их адресатам,
//     которых API уже посчитал.
//
// Чего он не делает намеренно: не хранит состояние и не решает, кому
// писать. Привязки, дневной потолок и «кого касается это событие» —
// в API. Своя копия любого из этих правил означала бы два ответа на
// один вопрос и однажды — два сообщения одному человеку.

import { createHmac, timingSafeEqual } from 'node:crypto';
import { createServer } from 'node:http';

import * as api from './api.js';
import { config, botNames } from './config.js';
import * as texts from './texts.js';
import { managerMessageFor } from './texts.js';
import { answerCallbackQuery, isBlockedError, sendMessage } from './telegram.js';
import { createCommentHandlers } from './comments.js';
import { createDeliveryMemory, deliverToPeople } from './delivery.js';
import { classifyUpdate } from './updates.js';

const log = (level, msg, extra = {}) => {
  // Ни токенов, ни секретов, ни сырого init_data: лог сервиса, который
  // держит ключи от двух ботов, не должен их пересказывать.
  console.log(JSON.stringify({ ts: new Date().toISOString(), level, msg, ...extra }));
};

// ── дедупликация доставки ──────────────────────────────────────────
//
// API повторяет отправку при любой нашей ошибке — так устроен outbox,
// и это правильно. Значит одно и то же событие приходит дважды, и
// человек получает два одинаковых сообщения, если мы не помним, что
// уже отправили.
//
// Память процессная и ограниченная: сервис перезапускается редко, а
// окно повторов — минуты. Вечная память здесь не нужна, а внешнее
// хранилище — ещё одна зависимость ради защиты от дубля.
//
// Помним не «событие сделано», а «в какой чат какого события ушло и под
// каким message_id» — подробности и почему так в delivery.js.
const delivery = createDeliveryMemory();

// ── вебхук Telegram ────────────────────────────────────────────────

async function onUpdate(bot, update) {
  // Что это за апдейт, решает чистая функция (updates.js): её можно
  // проверить скриптом, а ошибка в ней — команда, записанная в проект
  // комментарием, — тихая и дорогая.
  const u = classifyUpdate(update);
  switch (u.kind) {
    case 'blocked': {
      // Человек заблокировал бота. Говорим API: иначе мы будем слать
      // ему в пустоту и тратить его дневной лимит.
      const res = await api.blocked(bot, u.tgUserID);
      log('info', 'bot blocked by user', { bot, status: res.status });
      return;
    }
    case 'chatid':
      // Группы. Бот живёт в общем чате менеджеров и разговаривать там
      // не должен: он туда пишет, а не отвечает. Единственное
      // исключение — /chatid: идентификатор группы иначе неоткуда
      // взять, а без него сообщения менеджерам отправлять некуда.
      await sendMessage(
        bot,
        u.chatID,
        `TELEGRAM_MANAGERS_CHAT_ID=${u.chatID}` +
          (u.thread ? `\nTELEGRAM_MANAGERS_THREAD_ID=${u.thread}` : ''),
        u.thread ? { message_thread_id: u.thread } : {},
      );
      log('info', 'chatid asked', { bot, chat_type: u.chatType });
      return;
    case 'start':
      await onStart(bot, u);
      return;
    case 'command': {
      // Бот не ведёт переписку командами: справка, и всё.
      const who = await api.whoIs(bot, u.tgUserID);
      const known = who.status === 200;
      await sendMessage(bot, u.chatID, known ? texts.help[bot] : `${texts.help[bot]}\n\n${texts.notLinked}`);
      return;
    }
    case 'comment':
      await comments.onComment(bot, u);
      return;
    case 'plain':
      // Не ответ ни на что: в проект не пишем, подсказываем как.
      await comments.onPlain(bot, u);
      return;
    case 'media':
      await comments.onMedia(bot, u);
      return;
    case 'write':
      await comments.onWriteButton(bot, u);
      return;
    case 'callback':
      await answerCallbackQuery(bot, u.callbackID).catch(() => {});
      return;
    default:
  }
}

async function onStart(bot, u) {
  if (!u.code) {
    // /start без кода: рассказываем, что это, и ведём в кабинет.
    // Заводить аккаунт по одному нажатию нельзя — у человека уже
    // может быть наш, и второй пустой оставит его без проектов.
    await sendMessage(bot, u.chatID, `${texts.help[bot]}\n\n${texts.notLinked}`);
    return;
  }
  const res = await api.link(bot, u.code, { id: u.tgUserID, chatID: u.chatID, username: u.username });
  if (res.status === 200) {
    await sendMessage(bot, u.chatID, texts.linked);
    log('info', 'linked', { bot });
    return;
  }
  const reason = res.data?.error || 'not_found';
  await sendMessage(bot, u.chatID, texts.linkFailed[reason] || texts.linkFailed.not_found);
  log('warn', 'link failed', { bot, status: res.status, reason });
}

// Ответ боту → комментарий в проект; кнопка «Написать в проект».
// Логика — в comments.js: там же и обработка сбоев сети.
const comments = createCommentHandlers({ api, sendMessage, answerCallbackQuery, log });

// ── уведомления от API ─────────────────────────────────────────────

function signatureOK(raw, header) {
  if (!config.notifySecret) return true;
  const want = createHmac('sha256', config.notifySecret).update(raw).digest('hex');
  const got = String(header || '');
  if (got.length !== want.length) return false;
  return timingSafeEqual(Buffer.from(want), Buffer.from(got));
}

async function onNotify(envelope) {
  const {
    event_id: eventID,
    event_type: eventType,
    bot,
    audience = 'person',
    recipients = [],
    data = {},
    replyable,
  } = envelope;
  if (!bot || !config.bots[bot]) {
    // Бот не настроен — это НЕ ошибка доставки: второй бот может быть
    // ещё не выкачен. Ответим 200, иначе API будет ретраить вечно.
    log('warn', 'notify for unconfigured bot', { bot, event: eventType });
    return { ok: true, sent: 0 };
  }
  // Куда это событие уже ушло (пусто — впервые). Помечаем чат ПОСЛЕ
  // успешной отправки, а не событие до неё: упавшая отправка должна
  // повториться, а не потеряться как «дубль».
  const done = delivery.done(eventID);

  const app = envelope.app_base_url || config.appBaseURL;

  // Общий чат менеджеров. Получателей здесь нет и не нужно: чат один,
  // и знает о нём только сервис бота — API решает, ЧТО отправить, а
  // «в какой чат» вопрос доставки.
  if (audience === 'managers') {
    if (!config.managers.chatID) {
      // Чат не настроен — это НЕ ошибка доставки: бота могли ещё не
      // добавить в группу. Ретраи её не вылечат, а 500 стоил бы
      // десяти попыток на каждое событие.
      log('warn', 'managers chat is not configured', { event: eventType });
      return { ok: true, sent: 0 };
    }
    const text = managerMessageFor(eventType, data, app);
    if (!text) {
      log('warn', 'no manager text for event', { event: eventType });
      return { ok: true, sent: 0 };
    }
    if (done.has('managers')) {
      log('info', 'notify duplicate ignored', { event: eventType, event_id: eventID });
      return { ok: true, sent: 0, duplicate: true };
    }
    const extra = config.managers.threadID
      ? { message_thread_id: Number(config.managers.threadID) }
      : {};
    await sendMessage(config.managers.bot, config.managers.chatID, text, extra);
    done.set('managers', 0);
    log('info', 'notify managers', { event: eventType, sent: 1 });
    return { ok: true, sent: 1 };
  }

  // Под какими message_id ушли сообщения: API по ним узнает, к какому
  // проекту относится ответ человека. Сами мы этого не храним (память
  // доставки — только на окно повторов). Отправка кому-то упала —
  // deliverToPeople бросит PartialDeliveryError с message_id успешных,
  // и pollOnce отдаст их API вместе с failed.
  // Флага нет у конвертов от API без него (очередь до выкатки, или
  // бот выкачен раньше API) — тогда прежнее правило, по типу события.
  const canReply = typeof replyable === 'boolean' ? replyable : eventType.startsWith('project.');
  const markup = texts.notifyMarkup(app, eventType, data, canReply);
  const res = await deliverToPeople({
    recipients,
    done,
    send: (r) => {
      const text = texts.messageFor(eventType, data, app, r);
      if (!text) {
        // Неизвестный тип: писать человеку «project.foo» нельзя, а
        // ретраить нечего — текста не появится.
        log('warn', 'no text for event', { event: eventType });
        return null;
      }
      // Кнопка «Открыть» ведёт в мини-апп: обычная ссылка открыла бы
      // сайт во внешнем браузере, где сессии нет и человек упирается
      // в форму входа — ради собственной же выкладки.
      // Рядом — «Написать в проект»: ответить на пинг можно и просто
      // ответом на сообщение, но кнопку видно, а свайп — нет.
      return sendMessage(bot, r.tg_chat_id, text, markup ? { reply_markup: markup } : {});
    },
    isBlocked: isBlockedError,
    // Заблокировал — сообщаем API и идём дальше: остальные получатели
    // не виноваты.
    onBlocked: (r) => api.blocked(bot, r.tg_chat_id),
  });
  const duplicate = recipients.length > 0 && res.skipped === recipients.length;
  log('info', duplicate ? 'notify duplicate ignored' : 'notify', {
    event: eventType, event_id: eventID, bot, sent: res.sent, blocked: res.blocked, skipped: res.skipped,
  });
  return {
    ok: true, sent: res.sent, blocked: res.blocked, messages: res.messages,
    ...(duplicate ? { duplicate: true } : {}),
  };
}

// ── http ───────────────────────────────────────────────────────────

const readBody = (req) =>
  new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });

const json = (res, code, body) => {
  res.writeHead(code, { 'content-type': 'application/json' });
  res.end(JSON.stringify(body));
};

const server = createServer(async (req, res) => {
  try {
    if (req.method === 'GET' && (req.url === '/health' || req.url === '/')) {
      json(res, 200, { ok: true, bots: botNames });
      return;
    }

    // Вебхук Telegram: /webhook/<bot>/<secret>. Апдейты приходят без
    // авторизации, и секрет в пути — единственное, что отличает
    // Telegram от любого, кто знает адрес.
    const hook = /^\/webhook\/(creator|client)\/([^/]+)$/.exec(req.url || '');
    if (req.method === 'POST' && hook) {
      const [, bot, secret] = hook;
      if (secret !== config.webhookSecret) {
        json(res, 404, { error: 'not_found' });
        return;
      }
      const raw = await readBody(req);
      // Отвечаем Telegram СРАЗУ: он ретраит по таймауту, а наши походы
      // в API занимают сотни миллисекунд.
      json(res, 200, { ok: true });
      try {
        await onUpdate(bot, JSON.parse(raw.toString() || '{}'));
      } catch (err) {
        log('error', 'update failed', { bot, err: String(err?.message || err) });
      }
      return;
    }

    // Уведомления от API — СТАРЫЙ путь, оставлен как запасной.
    //
    // Основной теперь обратный: мы сами опрашиваем очередь (см.
    // pollOnce ниже). Причина — API живёт на российской ВДС, и
    // дозвониться оттуда сюда получается не всегда: первого октября
    // 2026 маршрут до Railway оборвался внутри сети хостера, и
    // уведомление потерялось. Ручку не убираем: она рабочая, стоит
    // дёшево и пригодится, если однажды направление станет надёжным.
    if (req.method === 'POST' && req.url === '/notify') {
      const raw = await readBody(req);
      if (config.notifyToken) {
        const got = String(req.headers.authorization || '').replace(/^Bearer\s+/i, '');
        if (got !== config.notifyToken) {
          json(res, 401, { error: 'bad_token' });
          return;
        }
      }
      if (!signatureOK(raw, req.headers['x-signature'])) {
        json(res, 401, { error: 'bad_signature' });
        return;
      }
      let envelope;
      try {
        envelope = JSON.parse(raw.toString() || '{}');
      } catch {
        // Битое тело — 400: ретраи его не вылечат, и API отправит
        // событие в DLQ вместо десяти попыток.
        json(res, 400, { error: 'bad_json' });
        return;
      }
      try {
        json(res, 200, await onNotify(envelope));
      } catch (err) {
        // 502 — транзиентно: API повторит, мы дедуплицируем по
        // event_id, и человек не получит дубль.
        log('error', 'notify failed', { err: String(err?.message || err) });
        json(res, 502, { error: 'delivery_failed' });
      }
      return;
    }

    json(res, 404, { error: 'not_found' });
  } catch (err) {
    log('error', 'request failed', { err: String(err?.message || err) });
    json(res, 500, { error: 'internal' });
  }
});

if (!config.notifySecret) {
  // Не тревога: основной путь — опрос очереди, он защищён общим
  // секретом BOT_SHARED_SECRET. Подпись нужна только запасной ручке
  // /notify, в которую сейчас никто не стучится.
  log('info', 'BOT_WEBHOOK_SECRET пуст: запасная ручка /notify без подписи');
}
if (botNames.length === 0) {
  log('warn', 'ни один бот не настроен: TELEGRAM_*_BOT_TOKEN пусты');
}
if (!config.managers.chatID) {
  log(
    'warn',
    'TELEGRAM_MANAGERS_CHAT_ID пуст: сообщения менеджерам никуда не уйдут. ' +
      'Добавьте бота в группу и отправьте там /chatid',
  );
}
/**
 * Опрос очереди: раз в несколько секунд спрашиваем API, есть ли что
 * отправить, отправляем и квитируем.
 *
 * Почему так, а не «API присылает нам». API живёт на российской ВДС,
 * мы — в чужом облаке, и дозвониться оттуда сюда выходит не всегда:
 * первого октября 2026 уведомление потерялось именно так — сервис
 * спал, холодный старт шёл дольше таймаута, все попытки оборвались, и
 * событие уехало в мёртвую очередь. Обратное направление работает
 * всегда: за привязкой и пользователями мы ходим туда сами, на этом
 * живёт мини-апп.
 *
 * Выдача — аренда: пока мы не подтвердили доставку, сообщение
 * остаётся в очереди и через минуту вернётся следующему опросу.
 * Поэтому упавший контейнер ничего не теряет, а от повторной отправки
 * защищает дедуп по event_id — тот же, что был у входящих.
 */
let polling = false;

async function pollOnce() {
  if (polling) return;
  polling = true;
  try {
    const { status, data } = await api.pullMessages(20);
    if (status !== 200) {
      log('warn', 'pull failed', { status });
      return;
    }
    const items = (data && data.items) || [];
    if (!items.length) return;

    const delivered = [];
    const failed = [];
    const sent = [];
    for (const m of items) {
      try {
        const res = await onNotify(m.envelope || {});
        delivered.push(m.id);
        for (const x of res?.messages || []) sent.push({ id: m.id, ...x });
      } catch (err) {
        // Не доставили — возвращаем в очередь с причиной. Следующий
        // опрос возьмёт его снова; телеграм падает редко, но когда
        // падает, терять сообщение нельзя.
        failed.push({ id: m.id, error: String(err?.message || err).slice(0, 400) });
        // Но кому-то из адресатов оно, может быть, уже ушло: их
        // message_id отдаём API и при failed — он запомнит, к какому
        // проекту ответ на них. Повтор этим адресатам не пошлёт (память
        // доставки), зато снова сообщит их номера — API это не смутит.
        for (const x of err?.messages || []) sent.push({ id: m.id, ...x });
      }
    }
    await api.ackMessages(delivered, failed, sent);
    log('info', 'pulled', { got: items.length, sent: delivered.length, failed: failed.length });
  } catch (err) {
    // Сеть моргнула — следующий тик попробует снова. Шуметь в лог на
    // каждую такую мелочь незачем: очередь никуда не денется.
    log('warn', 'poll failed', { err: String(err?.message || err) });
  } finally {
    polling = false;
  }
}

server.listen(config.port, () => log('info', 'listening', { port: config.port, bots: botNames }));

if (config.api.baseURL && config.api.secret) {
  const every = Math.max(1, config.api.pollSeconds) * 1000;
  setInterval(pollOnce, every).unref?.();
  log('info', 'очередь уведомлений: опрашиваем API', { every_ms: every });
} else {
  log('warn', 'API_BASE_URL/BOT_SHARED_SECRET пусты: очередь уведомлений не опрашивается');
}
