// Тонкая обёртка над Bot API. Ни библиотеки, ни зависимостей: нам
// нужны три метода из ста, а каждая зависимость в сервисе, который
// держит токены двух ботов, — это ещё один способ их потерять.

import { config } from './config.js';

const API = 'https://api.telegram.org/bot';

async function call(bot, method, body) {
  const token = config.bots[bot];
  if (!token) throw new Error(`bot ${bot} is not configured`);
  const res = await fetch(`${API}${token}/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!data.ok) {
    // Текст ошибки Telegram полезен (в нём «bot was blocked by the
    // user» или «chat not found»), а токен в него не попадает.
    const err = new Error(`telegram ${method}: ${data.description || res.status}`);
    err.code = data.error_code;
    err.description = data.description || '';
    throw err;
  }
  return data.result;
}

/**
 * Отправить сообщение.
 *
 * parse_mode=Markdown намеренно не ставим по умолчанию: тексты
 * собираются из данных проекта (названия, имена), и незакрытая
 * звёздочка в названии роняет сообщение целиком — Telegram отвечает
 * 400, и человек не получает ничего.
 */
export function sendMessage(bot, chatID, text, extra = {}) {
  return call(bot, 'sendMessage', {
    chat_id: chatID,
    text,
    disable_web_page_preview: true,
    ...extra,
  });
}

export function setWebhook(bot, url) {
  return call(bot, 'setWebhook', {
    url,
    allowed_updates: ['message', 'my_chat_member', 'callback_query'],
    drop_pending_updates: true,
  });
}

export function getMe(bot) {
  return call(bot, 'getMe', {});
}

/** Человек заблокировал бота — Telegram отвечает именно так. */
export function isBlockedError(err) {
  const d = String(err?.description || '').toLowerCase();
  return d.includes('blocked by the user') || d.includes('user is deactivated') ||
    d.includes('chat not found');
}
