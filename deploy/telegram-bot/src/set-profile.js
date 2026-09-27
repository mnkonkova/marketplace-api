// Описания и команды ботов. Ставятся один раз через Bot API: аватар
// так не поставить (его умеет только @BotFather), а текст — можно.
const bots = {
  creator: {
    token: process.env.TELEGRAM_CREATOR_BOT_TOKEN,
    name: 'PrMarket · креаторы',
    short: 'Сроки выкладок, заявки и решения по роликам.',
    full:
      'Бот «Сотки» для креаторов.\n\n' +
      'Присылает сроки выкладок, новые заявки от заказчиков и решения по сданным роликам. ' +
      'Сама работа — в кабинете: бот сообщает, что в нём появилось.',
    commands: [{ command: 'start', description: 'Подключить уведомления' }],
  },
  client: {
    token: process.env.TELEGRAM_CLIENT_BOT_TOKEN,
    name: 'PrMarket · заказчики',
    short: 'Новые ролики, сдвиги дат и сводка по проекту.',
    full:
      'Бот «Сотки» для заказчиков.\n\n' +
      'Присылает вышедшие ролики, переносы дат и недельную сводку по проекту. ' +
      'Подробности и переписка с менеджером — в кабинете.',
    commands: [{ command: 'start', description: 'Подключить уведомления' }],
  },
};

const call = async (token, method, body) => {
  const res = await fetch(`https://api.telegram.org/bot${token}/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  const d = await res.json();
  if (!d.ok) throw new Error(`${method}: ${d.description}`);
};

for (const [name, b] of Object.entries(bots)) {
  if (!b.token) continue;
  await call(b.token, 'setMyName', { name: b.name });
  await call(b.token, 'setMyShortDescription', { short_description: b.short });
  await call(b.token, 'setMyDescription', { description: b.full });
  await call(b.token, 'setMyCommands', { commands: b.commands });
  console.log(`${name}: имя, описание и команды прописаны`);
}
