// Прописать вебхуки обоим ботам. Запускается руками после выката:
//
//   PUBLIC_URL=https://prmarket-bots.up.railway.app npm run set-webhook
//
// Адрес вебхука содержит секрет (см. server.js), поэтому в лог печатаем
// только имя бота и результат — не сам адрес.

import { config, botNames } from './config.js';
import { getMe, setWebhook } from './telegram.js';

const base = (process.env.PUBLIC_URL || '').replace(/\/+$/, '');
if (!base) {
  console.error('env PUBLIC_URL is required');
  process.exit(1);
}

for (const bot of botNames) {
  const me = await getMe(bot);
  await setWebhook(bot, `${base}/webhook/${bot}/${config.webhookSecret}`);
  console.log(`${bot}: @${me.username} — вебхук прописан`);
}
if (botNames.length === 0) console.error('ни один бот не настроен');
