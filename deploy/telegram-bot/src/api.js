// Ходим в API «PrMarket». Источник истины там: кто человек, к какому
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

/**
 * Забрать сообщения для отправки.
 *
 * Доставка перевёрнута: не API стучится к нам, а мы спрашиваем его
 * сами. Причина проста — API живёт на российской ВДС, мы в чужом
 * облаке, и дозвониться оттуда сюда получается не всегда. А отсюда
 * туда — всегда: на этом же направлении работают привязка и мини-апп.
 */
export function pullMessages(limit = 20) {
  return request('GET', `/api/v1/bot/messages?limit=${limit}`);
}

/**
 * Что с пачкой: доставлено / не доставлено и почему.
 *
 * sent — [{ id, chat_id, message_id }]: под каким номером ушло каждое
 * личное сообщение. Сами мы его не храним — запоминает API, и по нему
 * ответ человека на пинг становится комментарием в проекте.
 */
export function ackMessages(delivered, failed, sent = []) {
  return request('POST', '/api/v1/bot/messages/ack', { delivered, failed, sent });
}

/**
 * Человек написал боту — записать в проект. Какой проект и можно ли,
 * решает API: по reply_to_message_id или по последнему пингу.
 */
export function comment(bot, { tgUserID, chatID, replyTo, text }) {
  return request('POST', '/api/v1/bot/comments', {
    bot,
    tg_user_id: tgUserID,
    chat_id: chatID,
    reply_to_message_id: replyTo || 0,
    text,
  });
}

/**
 * Кнопка «Написать в проект». Без messageID — только проверить доступ
 * и узнать название; с messageID — ещё и запомнить это сообщение бота:
 * ответ на него уйдёт в этот проект.
 */
export function commentAnchor(bot, { tgUserID, chatID, projectID, messageID }) {
  return request('POST', '/api/v1/bot/comments/anchor', {
    bot,
    tg_user_id: tgUserID,
    chat_id: chatID,
    project_id: projectID,
    message_id: messageID || 0,
  });
}
