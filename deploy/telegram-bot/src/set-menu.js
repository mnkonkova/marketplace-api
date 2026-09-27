// Кнопка меню бота: вместо «Меню» с командами — вход в мини-апп.
//
// Так человек попадает в кабинет одним нажатием и внутри Telegram, а
// не через внешний браузер, где сессии нет.
const app = process.env.APP_BASE_URL || 'https://wayprmarket.ru';
const bots = {
  creator: { token: process.env.TELEGRAM_CREATOR_BOT_TOKEN, text: 'Мои проекты' },
  client: { token: process.env.TELEGRAM_CLIENT_BOT_TOKEN, text: 'Мои проекты' },
};
for (const [name, b] of Object.entries(bots)) {
  if (!b.token) continue;
  const res = await fetch(`https://api.telegram.org/bot${b.token}/setChatMenuButton`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      menu_button: {
        type: 'web_app',
        text: b.text,
        web_app: { url: `${app}/tg/${name}` },
      },
    }),
  });
  const d = await res.json();
  console.log(name, d.ok ? 'кнопка меню → мини-апп' : `ОШИБКА: ${d.description}`);
}
