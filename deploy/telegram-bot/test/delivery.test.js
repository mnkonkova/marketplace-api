// Рассылка события: частичный провал не теряет message_id успешных, а
// повтор шлёт только тем, кому не ушло.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { PartialDeliveryError, createDeliveryMemory, deliverToPeople } from '../src/delivery.js';

const recipients = [{ tg_chat_id: 1 }, { tg_chat_id: 2 }, { tg_chat_id: 3 }];
const blockedErr = Object.assign(new Error('blocked'), { blocked: true });

function telegram(failFor = new Set()) {
  const calls = [];
  let id = 100;
  return {
    calls,
    send: async (r) => {
      calls.push(r.tg_chat_id);
      if (failFor.has(r.tg_chat_id)) throw failFor.get?.(r.tg_chat_id) || new Error('telegram 502');
      id += 1;
      return { message_id: id };
    },
  };
}

const opts = (done, send) => ({
  recipients, done, send, isBlocked: (e) => Boolean(e?.blocked), onBlocked: async () => {},
});

test('всё ушло — message_id каждого', async () => {
  const mem = createDeliveryMemory();
  const tg = telegram();
  const res = await deliverToPeople(opts(mem.done('e1'), tg.send));
  assert.equal(res.sent, 3);
  assert.deepEqual(res.messages.map((m) => m.chat_id), [1, 2, 3]);
});

test('один упал — остальным шлём, успешные в ошибке, повтор шлёт только ему', async () => {
  const mem = createDeliveryMemory();
  const flaky = telegram(new Set([2]));
  const err = await deliverToPeople(opts(mem.done('e2'), flaky.send)).catch((e) => e);
  assert.ok(err instanceof PartialDeliveryError);
  assert.deepEqual(flaky.calls, [1, 2, 3], 'третий не виноват в сбое второго');
  assert.deepEqual(err.messages, [
    { chat_id: 1, message_id: 101 },
    { chat_id: 3, message_id: 102 },
  ]);

  // Повтор того же события: дубля первому и третьему нет, их номера
  // снова в messages (прошлая квитанция могла потеряться).
  const ok = telegram();
  const res = await deliverToPeople(opts(mem.done('e2'), ok.send));
  assert.deepEqual(ok.calls, [2]);
  assert.equal(res.sent, 1);
  assert.equal(res.skipped, 2);
  assert.deepEqual(res.messages.map((m) => m.chat_id).sort(), [1, 2, 3]);
  assert.equal(res.messages.find((m) => m.chat_id === 1).message_id, 101);
});

test('полный повтор — никому не шлём, но номера отдаём', async () => {
  const mem = createDeliveryMemory();
  await deliverToPeople(opts(mem.done('e3'), telegram().send));
  const again = telegram();
  const res = await deliverToPeople(opts(mem.done('e3'), again.send));
  assert.deepEqual(again.calls, []);
  assert.equal(res.skipped, 3);
  assert.equal(res.messages.length, 3);
});

test('заблокировавшему не шлём повторно и не считаем сбоем', async () => {
  const mem = createDeliveryMemory();
  const failFor = new Map([[2, blockedErr]]);
  const tg = telegram(failFor);
  const blocked = [];
  const res = await deliverToPeople({
    ...opts(mem.done('e4'), tg.send),
    onBlocked: async (r) => blocked.push(r.tg_chat_id),
  });
  assert.equal(res.blocked, 1);
  assert.deepEqual(blocked, [2]);
  const again = telegram();
  await deliverToPeople(opts(mem.done('e4'), again.send));
  assert.deepEqual(again.calls, []);
});

test('без event_id памяти нет — каждый раз заново', async () => {
  const mem = createDeliveryMemory();
  const tg = telegram();
  await deliverToPeople(opts(mem.done(''), tg.send));
  await deliverToPeople(opts(mem.done(''), tg.send));
  assert.equal(tg.calls.length, 6);
});
