// Разбор апдейтов Telegram — без сети и без состояния.
//
// Вынесено из server.js ради одного: решение «что это за апдейт и что с
// ним делать» должно проверяться скриптом, без токенов и без Telegram.
// Ошибка здесь тихая и дорогая — команда /start, записанная в проект
// как комментарий, или чужое сообщение в группе менеджеров, ставшее
// «ответом креатора».

// Кнопка «Написать в проект» несёт id проекта в callback_data.
// Префикс короткий: у Telegram лимит 64 байта, uuid — 36.
const WRITE_PREFIX = 'w:';
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** callback_data кнопки «Написать в проект». */
export function writeCallbackData(projectID) {
  return `${WRITE_PREFIX}${projectID}`;
}

/** id проекта из callback_data или null, если кнопка не наша. */
export function parseWriteCallback(data) {
  const s = String(data || '');
  if (!s.startsWith(WRITE_PREFIX)) return null;
  const id = s.slice(WRITE_PREFIX.length);
  return UUID_RE.test(id) ? id : null;
}

/**
 * Что это за апдейт. Возвращает { kind, ... }:
 *
 *   blocked  — человек заблокировал бота;
 *   chatid   — /chatid в группе (единственное, на что бот там отвечает);
 *   start    — /start [код] в личке;
 *   command  — любая другая команда в личке: справка, а не комментарий;
 *   comment  — ответ (reply) текстом в личке: уходит в API, и тот решает,
 *              к какому проекту он относится — или отказывает;
 *   plain    — текст в личке, не ответ ни на что: в проект не пишем,
 *              только подсказка, как написать (проект не угадываем);
 *   write    — нажата кнопка «Написать в проект»;
 *   callback — чужая или устаревшая кнопка: только погасить «часики»;
 *   ignore   — всё остальное.
 *
 * Группы не порождают comment никогда: бот живёт в общем чате
 * менеджеров, и ответ менеджера на сообщение бота — разговор менеджеров
 * между собой, а не комментарий от их имени в проект.
 */
export function classifyUpdate(update = {}) {
  if (update.my_chat_member) {
    const status = update.my_chat_member.new_chat_member?.status;
    const from = update.my_chat_member.from || {};
    if (status === 'kicked' || status === 'left') {
      return { kind: 'blocked', tgUserID: from.id };
    }
    return { kind: 'ignore' };
  }

  if (update.callback_query) {
    const cq = update.callback_query;
    const chat = cq.message?.chat || {};
    const projectID = parseWriteCallback(cq.data);
    // Кнопка из группы или с битыми данными — не наша работа, но
    // «часики» на кнопке погасить надо: иначе Telegram крутит их
    // человеку полминуты.
    if (!projectID || (chat.type && chat.type !== 'private')) {
      return { kind: 'callback', callbackID: cq.id };
    }
    return {
      kind: 'write',
      callbackID: cq.id,
      tgUserID: cq.from?.id,
      chatID: chat.id ?? cq.from?.id,
      projectID,
    };
  }

  const msg = update.message;
  if (!msg) return { kind: 'ignore' };
  const from = msg.from || {};
  const chatID = msg.chat?.id ?? from.id;
  const chatType = msg.chat?.type || 'private';
  const text = typeof msg.text === 'string' ? msg.text.trim() : '';

  if (chatType !== 'private') {
    if (text.startsWith('/chatid')) {
      return { kind: 'chatid', chatID, chatType, thread: msg.message_thread_id || 0 };
    }
    return { kind: 'ignore' };
  }

  // Фото, стикеры, голосовые: в проект пока пишем только текст.
  if (!text) return { kind: 'ignore' };

  if (text.startsWith('/start')) {
    return {
      kind: 'start',
      code: text.slice('/start'.length).trim(),
      tgUserID: from.id,
      chatID,
      username: from.username || '',
    };
  }
  // Любая команда — не комментарий: «/help» в переписке проекта
  // выглядел бы как сбой.
  if (text.startsWith('/')) {
    return { kind: 'command', tgUserID: from.id, chatID };
  }
  // Комментарий — только ответ на сообщение: по reply_to_message_id API
  // найдёт проект (или откажет, если такого сообщения не помнит). Просто
  // текст в API не отправляем вовсе — у человека бывает два проекта, и
  // угаданный не тот хуже подсказки.
  const replyTo = msg.reply_to_message?.message_id || 0;
  if (!replyTo) {
    return { kind: 'plain', tgUserID: from.id, chatID };
  }
  return { kind: 'comment', tgUserID: from.id, chatID, replyTo, text };
}
