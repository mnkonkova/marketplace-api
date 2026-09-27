// Настройки сервиса ботов.
//
// Ботов два, и у каждого свой токен: креаторский разговаривает с
// исполнителями, клиентский — с заказчиками. Менеджерского нет и не
// будет — всё, что адресовано менеджеру, уходит в общий чат менеджеров
// через CRM-вебхук API.
//
// Всё читается из окружения и НИЧЕГО не пишется в лог: в токене и в
// общем секрете нет ничего, что стоило бы увидеть в дампе логов
// Railway.

const required = (name) => {
  const v = process.env[name];
  if (!v) throw new Error(`env ${name} is required`);
  return v;
};

export const config = {
  port: Number(process.env.PORT || 8080),

  // Токены ботов. Достаточно одного: второй бот тогда просто не
  // отвечает, а не роняет сервис — выкатывать их по одному нормально.
  bots: {
    creator: process.env.TELEGRAM_CREATOR_BOT_TOKEN || '',
    client: process.env.TELEGRAM_CLIENT_BOT_TOKEN || '',
  },

  // Секрет пути вебхука Telegram: адрес /webhook/<bot>/<secret>.
  // Telegram шлёт апдейты без авторизации, и единственная защита —
  // то, что адрес знают только он и мы.
  webhookSecret: required('TELEGRAM_WEBHOOK_SECRET'),

  // API «Сотки»: кто человек, привязка, отклики.
  api: {
    baseURL: (process.env.API_BASE_URL || '').replace(/\/+$/, ''),
    // Общий секрет группы /api/v1/bot/*.
    secret: process.env.BOT_SHARED_SECRET || '',
  },

  // Входящие уведомления от API. Тело подписано HMAC-SHA256 этим
  // ключом в заголовке X-Signature; пусто — подпись не проверяется
  // (локальный запуск), и об этом сервис громко предупреждает.
  notifySecret: process.env.BOT_WEBHOOK_SECRET || '',
  notifyToken: process.env.BOT_WEBHOOK_TOKEN || '',

  // Адрес кабинета: ссылки в сообщениях ведут туда, где человек
  // сделает то, о чём его просят.
  appBaseURL: (process.env.APP_BASE_URL || 'https://sotka.io').replace(/\/+$/, ''),
};

export const botNames = Object.keys(config.bots).filter((b) => config.bots[b]);
