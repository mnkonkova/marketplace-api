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
import { isBlockedError, sendMessage } from './telegram.js';

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
const seen = new Map();
const SEEN_TTL_MS = 6 * 60 * 60 * 1000;
const SEEN_MAX = 5000;

function alreadyDone(eventID) {
  const now = Date.now();
  for (const [k, at] of seen) {
    if (now - at > SEEN_TTL_MS) seen.delete(k);
  }
  if (seen.has(eventID)) return true;
  if (seen.size >= SEEN_MAX) seen.delete(seen.keys().next().value);
  seen.set(eventID, now);
  return false;
}

// ── вебхук Telegram ────────────────────────────────────────────────

async function onUpdate(bot, update) {
  // Человек заблокировал бота (или разблокировал). Говорим API: иначе
  // мы будем слать ему в пустоту и тратить его дневной лимит.
  if (update.my_chat_member) {
    const status = update.my_chat_member.new_chat_member?.status;
    const from = update.my_chat_member.from;
    if (status === 'kicked' || status === 'left') {
      const res = await api.blocked(bot, from.id);
      log('info', 'bot blocked by user', { bot, status: res.status });
    }
    return;
  }

  const msg = update.message;
  if (!msg || !msg.text) return;
  const from = msg.from || {};
  const chatID = msg.chat?.id ?? from.id;
  const chatType = msg.chat?.type || 'private';
  const text = msg.text.trim();

  // Группы. Бот живёт в общем чате менеджеров и разговаривать там не
  // должен: он туда пишет, а не отвечает. Единственное исключение —
  // /chatid: идентификатор группы иначе неоткуда взять, а без него
  // сообщения менеджерам отправлять некуда.
  if (chatType !== 'private') {
    if (text.startsWith('/chatid')) {
      const thread = msg.message_thread_id;
      await sendMessage(
        bot,
        chatID,
        `TELEGRAM_MANAGERS_CHAT_ID=${chatID}` +
          (thread ? `\nTELEGRAM_MANAGERS_THREAD_ID=${thread}` : ''),
        thread ? { message_thread_id: thread } : {},
      );
      log('info', 'chatid asked', { bot, chat_type: chatType });
    }
    return;
  }

  if (text.startsWith('/start')) {
    const code = text.slice('/start'.length).trim();
    if (!code) {
      // /start без кода: рассказываем, что это, и ведём в кабинет.
      // Заводить аккаунт по одному нажатию нельзя — у человека уже
      // может быть наш, и второй пустой оставит его без проектов.
      await sendMessage(bot, chatID, `${texts.help[bot]}\n\n${texts.notLinked}`);
      return;
    }
    const res = await api.link(bot, code, { id: from.id, chatID, username: from.username });
    if (res.status === 200) {
      await sendMessage(bot, chatID, texts.linked);
      log('info', 'linked', { bot });
      return;
    }
    const reason = res.data?.error || 'not_found';
    await sendMessage(bot, chatID, texts.linkFailed[reason] || texts.linkFailed.not_found);
    log('warn', 'link failed', { bot, status: res.status, reason });
    return;
  }

  // Любое другое сообщение. Бот не ведёт переписку: работа живёт в
  // кабинете, и делать вид, что здесь можно что-то решить, нечестно.
  const who = await api.whoIs(bot, from.id);
  const known = who.status === 200;
  await sendMessage(bot, chatID, known ? texts.help[bot] : `${texts.help[bot]}\n\n${texts.notLinked}`);
}

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
  } = envelope;
  if (!bot || !config.bots[bot]) {
    // Бот не настроен — это НЕ ошибка доставки: второй бот может быть
    // ещё не выкачен. Ответим 200, иначе API будет ретраить вечно.
    log('warn', 'notify for unconfigured bot', { bot, event: eventType });
    return { ok: true, sent: 0 };
  }
  if (eventID && alreadyDone(eventID)) {
    log('info', 'notify duplicate ignored', { event: eventType, event_id: eventID });
    return { ok: true, sent: 0, duplicate: true };
  }

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
    const extra = config.managers.threadID
      ? { message_thread_id: Number(config.managers.threadID) }
      : {};
    await sendMessage(config.managers.bot, config.managers.chatID, text, extra);
    log('info', 'notify managers', { event: eventType, sent: 1 });
    return { ok: true, sent: 1 };
  }

  let sent = 0;
  let blocked = 0;
  for (const r of recipients) {
    const text = texts.messageFor(eventType, data, app, r);
    if (!text) {
      // Неизвестный тип: писать человеку «project.foo» нельзя, а
      // ретраить нечего — текста не появится.
      log('warn', 'no text for event', { event: eventType });
      break;
    }
    try {
      await sendMessage(bot, r.tg_chat_id, text);
      sent += 1;
    } catch (err) {
      if (isBlockedError(err)) {
        // Заблокировал — сообщаем API и идём дальше: остальные
        // получатели не виноваты.
        await api.blocked(bot, r.tg_chat_id).catch(() => {});
        blocked += 1;
        continue;
      }
      throw err;
    }
  }
  log('info', 'notify', { event: eventType, bot, sent, blocked });
  return { ok: true, sent, blocked };
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

    // Уведомления от API.
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
  log('warn', 'BOT_WEBHOOK_SECRET is empty: подпись уведомлений не проверяется');
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
server.listen(config.port, () => log('info', 'listening', { port: config.port, bots: botNames }));
