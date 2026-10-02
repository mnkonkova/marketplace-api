// Ответ боту → комментарий: что видит человек при отказах и сбоях сети.
//
// Без сети: API и Telegram — подделки, которые записывают вызовы.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { createCommentHandlers } from '../src/comments.js';
import * as texts from '../src/texts.js';

const PID = '0b6f2a3e-8d1c-4c55-9a77-2f1e3d4c5b6a';

function stand(api) {
  const sent = [];
  const answers = [];
  let nextID = 500;
  const h = createCommentHandlers({
    api,
    sendMessage: async (bot, chatID, text, extra) => {
      sent.push({ chatID, text, extra });
      nextID += 1;
      return { message_id: nextID };
    },
    answerCallbackQuery: async (bot, id, text = '', alert = false) => {
      answers.push({ id, text, alert });
    },
  });
  return { h, sent, answers };
}

const reply = { tgUserID: 7, chatID: 7, replyTo: 901, text: 'успею' };
const button = { callbackID: 'cb', tgUserID: 7, chatID: 7, projectID: PID };
const boom = () => Promise.reject(new Error('fetch failed'));

test('записано — называем проект и читателя из ответа API, подтверждение запоминаем', async () => {
  const anchors = [];
  const { h, sent } = stand({
    comment: async () => ({
      status: 201,
      data: { project_id: PID, project_title: 'Весна', thread: 'client', reader: 'executor' },
    }),
    commentAnchor: async (bot, x) => {
      anchors.push(x);
      return { status: 200 };
    },
  });
  await h.onComment('client', reply);
  assert.equal(sent[0].text, 'Записала в проект «Весна» — исполнитель увидит.');
  assert.equal(anchors[0].messageID, 501);
  assert.equal(anchors[0].projectID, PID);
});

test('ответ на неизвестное сообщение — отказ no_project', async () => {
  const { h, sent } = stand({ comment: async () => ({ status: 404, data: { error: 'no_project' } }) });
  await h.onComment('creator', reply);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].text, texts.commentRefused.no_project);
  assert.match(sent[0].text, /Не поняла, к какому проекту это/);
});

test('API недоступен или 5xx — «не получилось», а не молчание', async () => {
  for (const comment of [boom, async () => ({ status: 502, data: { raw: 'Bad Gateway' } })]) {
    const { h, sent } = stand({ comment });
    await h.onComment('creator', reply);
    assert.deepEqual(sent.map((m) => m.text), [texts.commentRefused.failed]);
  }
});

test('прочий некорректный ввод — не «сократите»', async () => {
  const { h, sent } = stand({ comment: async () => ({ status: 400, data: { error: 'invalid_comment' } }) });
  await h.onComment('creator', reply);
  assert.equal(sent[0].text, texts.commentRefused.invalid_comment);
  assert.notEqual(sent[0].text, texts.commentRefused.too_long);
});

test('простой текст — подсказка, в API как комментарий не уходит', async () => {
  let commented = false;
  const { h, sent } = stand({
    comment: async () => {
      commented = true;
      return { status: 201, data: {} };
    },
    whoIs: async () => ({ status: 200, data: {} }),
  });
  await h.onPlain('creator', { tgUserID: 7, chatID: 7 });
  assert.equal(commented, false);
  assert.deepEqual(sent.map((m) => m.text), [
    'Чтобы написать в проект, ответьте на сообщение о нём или нажмите „Написать в проект“ под уведомлением.',
  ]);

  // Не привязан — кнопки у него нет, ведём к привязке.
  const other = stand({ whoIs: async () => ({ status: 404 }) });
  await other.h.onPlain('creator', { tgUserID: 8, chatID: 8 });
  assert.match(other.sent[0].text, /Аккаунт не привязан/);

  // API лежит — подсказка всё равно уместна.
  const down = stand({ whoIs: boom });
  await down.h.onPlain('creator', { tgUserID: 9, chatID: 9 });
  assert.equal(down.sent[0].text, texts.writeHint);
});

test('файл или фото: подключённому — про ссылку, не подключённому — подключиться', async () => {
  const linked = stand({ whoIs: async () => ({ status: 200, data: {} }) });
  await linked.h.onMedia('creator', { tgUserID: 7, chatID: 7 });
  assert.equal(linked.sent[0].text, texts.mediaHint);

  // Сообщений о проектах у него нет — «ответьте на сообщение» мимо.
  const other = stand({ whoIs: async () => ({ status: 404 }) });
  await other.h.onMedia('creator', { tgUserID: 8, chatID: 8 });
  assert.match(other.sent[0].text, /Аккаунт не привязан/);
});

test('альбом — одна подсказка, а не по одной на снимок', async () => {
  const { h, sent } = stand({ whoIs: async () => ({ status: 200, data: {} }) });
  for (let i = 0; i < 6; i++) {
    await h.onMedia('creator', { tgUserID: 7, chatID: 7, group: 'g1' });
  }
  assert.equal(sent.length, 1);
  // Другой альбом и одиночное фото — уже отдельные сообщения.
  await h.onMedia('creator', { tgUserID: 7, chatID: 7, group: 'g2' });
  await h.onMedia('creator', { tgUserID: 7, chatID: 7, group: '' });
  assert.equal(sent.length, 3);
});

test('кнопка: сеть упала — отвечаем на нажатие «не получилось»', async () => {
  const { h, sent, answers } = stand({ commentAnchor: boom });
  await h.onWriteButton('creator', button);
  assert.equal(answers.length, 1);
  assert.equal(answers[0].text, texts.commentRefused.failed);
  assert.equal(answers[0].alert, true);
  assert.equal(sent.length, 0);
});

test('кнопка: отказ API — ответ на нажатие с причиной', async () => {
  const { h, answers } = stand({ commentAnchor: async () => ({ status: 403, data: { error: 'not_member' } }) });
  await h.onWriteButton('creator', button);
  assert.deepEqual(answers, [{ id: 'cb', text: texts.commentRefused.not_member, alert: true }]);
});

test('кнопка: приглашение отправлено и запомнено', async () => {
  const calls = [];
  const { h, sent, answers } = stand({
    commentAnchor: async (bot, x) => {
      calls.push(x);
      return { status: 200, data: { project_title: 'Весна' } };
    },
  });
  await h.onWriteButton('creator', button);
  assert.equal(answers.length, 1);
  assert.equal(answers[0].text, '');
  assert.equal(sent.length, 1);
  assert.equal(sent[0].extra.reply_markup.force_reply, true);
  assert.equal(calls[1].messageID, 501);
});

test('кнопка: приглашение не запомнилось — говорим сразу; на нажатие ответили один раз', async () => {
  let n = 0;
  const { h, sent, answers } = stand({
    commentAnchor: async () => {
      n += 1;
      if (n === 1) return { status: 200, data: { project_title: 'Весна' } };
      throw new Error('fetch failed');
    },
  });
  await h.onWriteButton('creator', button);
  assert.equal(answers.length, 1);
  assert.deepEqual(sent.map((m) => m.text), [texts.writePrompt('Весна'), texts.writePromptLost]);
});

test('кнопка: упала отправка приглашения — «часики» всё равно погашены', async () => {
  const answers = [];
  const h = createCommentHandlers({
    api: { commentAnchor: async () => ({ status: 200, data: { project_title: 'Весна' } }) },
    sendMessage: async () => {
      throw new Error('telegram down');
    },
    answerCallbackQuery: async (bot, id, text = '') => answers.push({ id, text }),
  });
  await assert.rejects(h.onWriteButton('creator', button));
  assert.equal(answers.length, 1);
});
