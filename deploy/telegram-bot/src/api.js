// Ходим в API «Сотки». Источник истины там: кто человек, к какому
// аккаунту привязан телеграм, что с заявкой. Бот ничего этого не
// хранит — своё состояние означало бы два ответа на один вопрос.

import { config } from './config.js';

async function request(method, path, body) {
  if (!config.api.baseURL) throw new Error('API_BASE_URL is not set');
  const res = await fetch(`${config.api.baseURL}${path}`, {
    method,
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${config.api.secret}`,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { raw: text };
  }
  return { status: res.status, data };
}

/** Привязать телеграм по коду из кабинета. */
export function link(bot, code, from) {
  return request('POST', '/api/v1/bot/link', {
    bot,
    code,
    tg_user_id: from.id,
    tg_chat_id: from.chatID,
    tg_username: from.username || '',
  });
}

/** Кто это. 404 — не привязан, и это нормальное состояние. */
export function whoIs(bot, tgUserID) {
  return request('GET', `/api/v1/bot/users/by-telegram/${tgUserID}?bot=${bot}`);
}

/** Человек заблокировал бота. */
export function blocked(bot, tgUserID) {
  return request('POST', '/api/v1/bot/blocked', { bot, tg_user_id: tgUserID });
}
