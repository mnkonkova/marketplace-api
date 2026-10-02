// Проверка разбора апдейтов и кнопок под уведомлением.
//
// Запуск: npm test (или node --test) из deploy/telegram-bot.
// Ни сети, ни токенов: проверяем решения, а не Telegram.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { classifyUpdate, parseWriteCallback, writeCallbackData } from '../src/updates.js';
import { commentSaved, notifyMarkup } from '../src/texts.js';

const PID = '0b6f2a3e-8d1c-4c55-9a77-2f1e3d4c5b6a';
const me = { id: 4825, username: 'marina' };
const privateChat = { id: 4825, type: 'private' };
const group = { id: -100123, type: 'supergroup' };

test('ответ на пинг в личке — комментарий с reply_to', () => {
  const u = classifyUpdate({
    message: {
      from: me, chat: privateChat, text: '  успею к вечеру ',
      reply_to_message: { message_id: 901 },
    },
  });
  assert.deepEqual(u, { kind: 'comment', tgUserID: 4825, chatID: 4825, replyTo: 901, text: 'успею к вечеру' });
});

test('простое сообщение — не комментарий, а подсказка', () => {
  // Проект не угадываем: простой текст в API как комментарий не уходит.
  const u = classifyUpdate({ message: { from: me, chat: privateChat, text: 'вопрос' } });
  assert.deepEqual(u, { kind: 'plain', tgUserID: 4825, chatID: 4825 });
});

test('/start и другие команды комментариями не становятся', () => {
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, text: '/start abc' } }).kind, 'start');
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, text: '/start abc' } }).code, 'abc');
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, text: '/help' } }).kind, 'command');
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, text: '/chatid' } }).kind, 'command');
});

test('группа менеджеров: ответы не комментарии, /chatid отвечает', () => {
  const reply = classifyUpdate({
    message: { from: me, chat: group, text: 'беру', reply_to_message: { message_id: 5 } },
  });
  assert.equal(reply.kind, 'ignore');
  const chatid = classifyUpdate({ message: { from: me, chat: group, text: '/chatid', message_thread_id: 7 } });
  assert.deepEqual(chatid, { kind: 'chatid', chatID: -100123, chatType: 'supergroup', thread: 7 });
});

test('файл или фото — не молчим, а подсказываем', () => {
  // Скан подписанного договора ответом на пинг: без ответа бота человек
  // решит, что его записали.
  const scan = classifyUpdate({
    message: { from: me, chat: privateChat, photo: [{}], caption: 'подписала', reply_to_message: { message_id: 5 } },
  });
  assert.deepEqual(scan, { kind: 'media', tgUserID: 4825, chatID: privateChat.id, group: '' });
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, document: {} } }).kind, 'media');
  // В группе по-прежнему молчим.
  assert.equal(classifyUpdate({ message: { from: me, chat: group, photo: [{}] } }).kind, 'ignore');
});

test('не текст и пустое — пропускаем', () => {
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, sticker: {} } }).kind, 'ignore');
  assert.equal(classifyUpdate({ message: { from: me, chat: privateChat, text: '   ' } }).kind, 'ignore');
  assert.equal(classifyUpdate({}).kind, 'ignore');
});

test('блокировка бота', () => {
  const u = classifyUpdate({ my_chat_member: { from: me, new_chat_member: { status: 'kicked' } } });
  assert.deepEqual(u, { kind: 'blocked', tgUserID: 4825 });
  assert.equal(
    classifyUpdate({ my_chat_member: { from: me, new_chat_member: { status: 'member' } } }).kind,
    'ignore',
  );
});

test('кнопка «Написать в проект»', () => {
  const data = writeCallbackData(PID);
  assert.ok(Buffer.byteLength(data) <= 64, 'callback_data длиннее 64 байт');
  assert.equal(parseWriteCallback(data), PID);
  assert.equal(parseWriteCallback('w:не-uuid'), null);
  assert.equal(parseWriteCallback('x:' + PID), null);

  const u = classifyUpdate({
    callback_query: { id: 'cb1', from: me, data, message: { message_id: 3, chat: privateChat } },
  });
  assert.deepEqual(u, { kind: 'write', callbackID: 'cb1', tgUserID: 4825, chatID: 4825, projectID: PID });

  // Из группы или чужая — только погасить часики.
  assert.deepEqual(
    classifyUpdate({ callback_query: { id: 'cb2', from: me, data, message: { chat: group } } }),
    { kind: 'callback', callbackID: 'cb2' },
  );
  assert.equal(classifyUpdate({ callback_query: { id: 'cb3', from: me, data: 'zzz' } }).kind, 'callback');
});

test('под пингом проекта — «Открыть» и «Написать в проект»', () => {
  const m = notifyMarkup('https://app', 'project.publication_due_today', { project_id: PID }, true);
  assert.equal(m.inline_keyboard.length, 1);
  const [open, write] = m.inline_keyboard[0];
  assert.equal(open.text, 'Открыть');
  assert.ok(open.web_app.url.startsWith('https://app/tg/creator?to='));
  assert.deepEqual(write, { text: 'Написать в проект', callback_data: writeCallbackData(PID) });

  const client = notifyMarkup('https://app', 'project.client_new_video', { project_id: PID }, true);
  assert.equal(client.inline_keyboard[0][1].callback_data, writeCallbackData(PID));
});

test('у заявки без проекта — только «Открыть»', () => {
  const m = notifyMarkup('https://app', 'order.broadcast_sent', {});
  assert.equal(m.inline_keyboard[0].length, 1);
  assert.equal(m.inline_keyboard[0][0].text, 'Открыть');
  assert.equal(notifyMarkup('https://app', 'project.unknown', {}), undefined);
});

test('API не пометил конверт отвечаемым — без «Написать в проект»', () => {
  // Рассылка заявки: project_id есть, но получатели в проекте ещё не
  // состоят — на заявку отвечают откликом. Решает API, не бот.
  const m = notifyMarkup('https://app', 'order.broadcast_sent', { project_id: PID }, false);
  assert.deepEqual(
    m.inline_keyboard[0].map((b) => b.text),
    ['Открыть'],
  );
});

test('подтверждение называет проект и читателя — того, кого назвал API', () => {
  assert.equal(commentSaved('Весна', 'manager'), 'Записала в проект «Весна» — менеджер увидит.');
  assert.equal(commentSaved('Весна', 'client'), 'Записала в проект «Весна» — заказчик увидит.');
  // Заказчик общего проекта: менеджера нет, читает исполнитель.
  assert.equal(commentSaved('Весна', 'executor'), 'Записала в проект «Весна» — исполнитель увидит.');
  // API старой версии читателя не сообщает — не называем никого.
  assert.equal(commentSaved('Весна', undefined), 'Записала в проект «Весна».');
});
