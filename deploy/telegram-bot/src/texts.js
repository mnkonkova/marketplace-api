// Тексты сообщений.
//
// Правила, по которым они написаны, короче самих текстов:
//
//   • сообщение отвечает на вопрос «что мне сделать», а не «что
//     случилось в системе». «Ролик вернули» без замечания — это повод
//     открыть кабинет и гадать;
//   • ссылка ведёт ровно туда, где это делается, а не на главную;
//   • никаких «уважаемый пользователь» и восклицательных знаков: это
//     рабочая переписка, а не рассылка;
//   • дата словами там, где она про срок («сегодня», «завтра»): «срок
//     2026-10-03» человек считает в уме.

const plural = (n, one, few, many) => {
  const mod10 = n % 10;
  const mod100 = n % 100;
  if (mod10 === 1 && mod100 !== 11) return one;
  if (mod10 >= 2 && mod10 <= 4 && (mod100 < 10 || mod100 >= 20)) return few;
  return many;
};

const day = (iso) => {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleDateString('ru-RU', { day: 'numeric', month: 'long' });
};

const money = (kopecks) => `${Math.round(Number(kopecks || 0) / 100).toLocaleString('ru-RU')} ₽`;

/**
 * Кнопка «Открыть» — она же вход в мини-апп.
 *
 * Обычная ссылка в сообщении открывает сайт во внешнем браузере: там
 * нет сессии, и человек упирается в форму входа — ради того, чтобы
 * посмотреть свою же выкладку. Кнопка web_app открывает мини-апп
 * внутри Telegram, вход в него делает подпись initData, и человек
 * сразу оказывается на нужном экране.
 *
 * Адрес ведёт на /tg, а не прямо в кабинет: сессии в мини-аппе ещё
 * нет, её выдаёт именно этот экран, а `to` говорит ему, куда идти
 * дальше.
 *
 * web_app-кнопки живут только в личке. Для группы менеджеров —
 * обычная ссылка (см. managerMessageFor): там Telegram их не
 * принимает вовсе.
 */
export function openButton(app, bot, path, label = 'Открыть') {
  if (!app || !path) return undefined;
  const url = `${app}/tg/${bot}?to=${encodeURIComponent(path)}`;
  return { inline_keyboard: [[{ text: label, web_app: { url } }]] };
}

/**
 * Куда ведёт кнопка у этого события. null — кнопки нет: экран, на
 * который нечего открывать, лучше без неё, чем с ведущей «куда-то».
 */
export function targetFor(eventType, d = {}) {
  const project = d.project_id;
  switch (eventType) {
    case 'project.publication_due_tomorrow':
    case 'project.publication_due_today':
    case 'project.publication_no_views':
    case 'project.publication_incomplete':
    case 'project.publication_manual':
    case 'project.publication_returned':
    case 'project.publication_accepted':
    case 'project.publication_moved':
    case 'project.publication_cancelled':
    case 'project.project_creator_briefed':
    case 'project.project_materials_updated':
    case 'project.project_checklist_updated':
      return project ? { bot: 'creator', path: `/me/creator/projects/${project}` } : null;
    case 'order.invitation_sent':
    case 'order.broadcast_sent':
      return { bot: 'creator', path: '/me/creator/invitations' };
    case 'project.client_new_video':
    case 'project.client_views_threshold':
    case 'project.client_date_shift':
    case 'project.client_weekly_digest':
      return project ? { bot: 'client', path: `/me/projects/${project}` } : null;
    default:
      return null;
  }
}

/**
 * Текст по событию. Возвращает null, если писать нечего: неизвестный
 * тип — это не повод отправить человеку «событие project.foo».
 *
 * app — адрес кабинета, recipient — кому пишем (нужен для приписки
 * «вас хотят особенно»).
 */
export function messageFor(eventType, d = {}, app, recipient = null) {
  const project = d.project_title || d.title || 'проект';
  const projectLink = d.project_id ? `${app}/me/creator/projects/${d.project_id}` : app;
  const clientLink = d.project_id ? `${app}/me/projects/${d.project_id}` : app;

  switch (eventType) {
    // ---- креатор ----
    case 'project.publication_due_tomorrow':
      return `Завтра выкладка · ${project}\nСрок: ${day(d.due_date)}\n${projectLink}`;
    case 'project.publication_due_today':
      return `Сегодня выкладка · ${project}\nКак выложите — пришлите ссылки в кабинете.\n${projectLink}`;
    case 'project.publication_incomplete': {
      const left = (d.missing_platforms || []).join(', ');
      return `Ролик вышел не везде · ${project}\n` +
        (left ? `Не хватает: ${left}\n` : '') +
        `Досылать площадки можно по одной.\n${projectLink}`;
    }
    case 'project.publication_manual':
      return `Напоминание от менеджера · ${project}\nСрок: ${day(d.due_date)}\n${projectLink}`;
    // Ноль на вторые сутки почти никогда не «никто не посмотрел»: чаще
    // это не тот адрес, теневой бан или закрытый аккаунт. Поэтому текст
    // не обвиняет и не хвалит, а зовёт проверить — и говорит, что
    // именно проверять.
    case 'project.publication_no_views': {
      const views = Number(d.views || 0);
      // Склонение руками: «41 просмотров» выдаёт машину, а письмо
      // должно читаться как от человека.
      const tail = views % 10 === 1 && views % 100 !== 11 ? 'просмотр'
        : [2, 3, 4].includes(views % 10) && ![12, 13, 14].includes(views % 100) ? 'просмотра'
          : 'просмотров';
      const head = views > 0
        ? `Ролик почти не смотрят · ${project}\nЗа двое суток ${views} ${tail}.`
        : `У ролика нет просмотров · ${project}\nЗа двое суток ноль.`;
      return `${head}\nПроверьте, открывается ли ссылка и виден ли ролик чужому аккаунту.\n${projectLink}`;
    }
    case 'project.publication_returned':
      return `Ролик вернули с замечанием · ${project}\n` +
        (d.comment ? `«${String(d.comment).slice(0, 400)}»\n` : '') +
        `${projectLink}`;
    case 'project.publication_accepted':
      return `Ролик принят · ${project}\n${projectLink}`;
    case 'project.publication_moved':
      return `Дату выкладки перенесли · ${project}\nНовый срок: ${day(d.due_date)}\n${projectLink}`;
    case 'project.publication_cancelled':
      return `Выкладку сняли с плана · ${project}\nБыло: ${day(d.due_date)}\n${projectLink}`;
    case 'project.project_creator_briefed':
      return `Вас добавили в проект · ${project}\n` +
        `Задание, договор и чек-лист — в кабинете.\n${projectLink}`;
    case 'project.project_materials_updated':
      return `Материалы проекта обновились · ${project}\n` +
        `Перед съёмкой загляните: снимать по вчерашнему заданию — это пересъёмка.\n${projectLink}`;
    case 'project.project_checklist_updated':
      return `Чек-лист сдачи изменился · ${project}\n${projectLink}`;

    case 'order.invitation_sent':
      return `Вас зовут в проект\n` +
        `Роликов в месяц: ${d.videos_count || '—'}\n` +
        `Ответить: ${app}/me/creator/invitations`;
    case 'order.broadcast_sent': {
      // Отметка «хочу особенно» — приписка в ТОМ ЖЕ сообщении, а не
      // второе письмо: два сообщения об одной заявке съедают дневной
      // лимит и выглядят беспорядком.
      const preferred = (d.preferred_ids || []).includes(recipient?.user_id);
      return `Новая заявка${preferred ? ' — заказчик хочет особенно вас' : ''}\n` +
        (d.title ? `${d.title}\n` : '') +
        `Роликов в месяц: ${d.videos_count || '—'}\n` +
        `Ответить роликом или своими работами: ${app}/me/creator/invitations`;
    }

    // ---- заказчик ----
    case 'project.client_new_video':
      return `Вышел новый ролик · ${project}\n${clientLink}`;
    case 'project.client_views_threshold':
      return `Ролик перешагнул ${Number(d.threshold || 0).toLocaleString('ru-RU')} просмотров · ${project}\n${clientLink}`;
    case 'project.client_date_shift':
      return `Дата выкладки сдвинулась · ${project}\nНовый срок: ${day(d.due_date)}\n${clientLink}`;
    case 'project.client_weekly_digest': {
      const published = Number(d.published || 0);
      return `Неделя по проекту · ${project}\n` +
        `Вышло ${published} ${plural(published, 'ролик', 'ролика', 'роликов')}` +
        (d.views ? `, просмотров ${Number(d.views).toLocaleString('ru-RU')}` : '') +
        (d.views_gained ? ` (+${Number(d.views_gained).toLocaleString('ru-RU')} за неделю)` : '') +
        `\n${clientLink}`;
    }

    default:
      return null;
  }
}

export const help = {
  creator:
    'Это бот «PrMarket» для креаторов.\n\n' +
    'Здесь приходят сроки выкладок, заявки от заказчиков и решения по роликам. ' +
    'Работа живёт в кабинете — бот только сообщает, что в нём появилось.',
  client:
    'Это бот «PrMarket» для заказчиков.\n\n' +
    'Здесь приходят новые ролики, сдвиги дат и недельная сводка по проекту. ' +
    'Подробности — в кабинете.',
};

export const linked = 'Готово: уведомления будут приходить сюда.';

export const notLinked =
  'Аккаунт не привязан.\n\n' +
  'Откройте кабинет, найдите в проекте кнопку «Получать уведомления в Telegram» ' +
  'и нажмите на ссылку оттуда — она приведёт сюда с кодом.';

export const linkFailed = {
  not_found: 'Такой ссылки у нас нет. Получите новую в кабинете.',
  code_expired: 'Ссылка устарела — она живёт пятнадцать минут. Получите новую в кабинете.',
  telegram_taken:
    'Этот телеграм уже привязан к другому аккаунту. Напишите менеджеру — перенесём.',
};

export { money, plural, day };

/**
 * Сообщение в общий чат менеджеров.
 *
 * Раньше эти тексты жили в Code-ноде n8n, и правка каждого была
 * правкой workflow в чужом интерфейсе — с копипастом JSON и без
 * ревью. Теперь они здесь, рядом с остальными.
 *
 * Правило то же, что у личных: сообщение отвечает на вопрос «что
 * делать», а не «что случилось в базе». Разница одна — адресат:
 * менеджеру нужна ссылка в CRM, а не в кабинет.
 */
export function managerMessageFor(eventType, d = {}, app) {
  const title = d.title || d.project_title || '(без названия)';
  const projectID = d.project_id || d.aggregate_id || '';
  const link = projectID && app ? `\n${app}/manager/projects/${projectID}` : '';
  const client = d.client_contact
    ? `${d.client_name || '—'} (${d.client_contact})`
    : d.client_name || '—';
  const spec = d.specialist_display_name || '—';
  const rubles = (kop) => `${Math.round(Number(kop || 0) / 100).toLocaleString('ru-RU')} ₽`;
  const num = (v) => Number(v || 0).toLocaleString('ru-RU');
  // Короткий id — у событий заявки: пока заявка не стала проектом,
  // отличить одну от другой в чате больше нечем.
  const short = (v) => String(v ?? '').slice(0, 8);

  switch (eventType) {
    case 'project.created':
      return `🆕 Новый бриф · ${title}\nКлиент: ${client}` +
        (d.budget ? `\nБюджет: ${d.budget} ₽` : '') + link;
    case 'project.general_created':
      return `🆕 Разовый заказ · ${title}\nКлиент: ${client}` +
        (spec !== '—' ? `\nИсполнитель: ${spec}` : '') +
        (d.due_date ? `\nСрок: ${day(d.due_date)}` : '') + link;
    case 'project.specialist_assigned':
      return `👤 Назначен специалист · ${title}\nКлиент: ${client}\nСпециалист: ${spec}` + link;
    case 'project.specialist_approved':
      return `✅ Менеджер одобрил специалиста · ${title}\nСпециалист: ${spec}` + link;
    case 'project.specialist_rejected':
      // Половина истории хуже, чем никакой: чат сообщал «клиент
      // выбрал» и молчал там, где подтверждения не будет.
      return `🚫 Менеджер отклонил специалиста · ${title}\nСпециалист: ${spec}` +
        (d.reason ? `\nПричина: ${d.reason}` : '') +
        `\nНужно предложить клиенту другого` + link;
    case 'project.assigned':
      return `👨‍💼 Менеджер взял в работу · ${title}\nКлиент: ${client}` +
        (d.manager_display_name ? `\nМенеджер: ${d.manager_display_name}` : '') + link;
    case 'project.comment_added':
      return `💬 Новый комментарий · ${title}\n${String(d.body || '').slice(0, 500)}` + link;
    case 'project.disputed':
      return `⚠️ Спор по проекту · ${title}\nКлиент: ${client}\nРабота остановлена — ответьте клиенту` + link;
    case 'project.publication_overdue':
      return `⏰ Просрочена выкладка · ${title}` +
        (d.due_date ? `\nСрок был ${day(d.due_date)}` : '') +
        (Number(d.days_overdue || 0) > 0 ? ` · просрочка ${d.days_overdue} дн.` : '') +
        `\nКреатор не выложил — нужен менеджер` + link;
    case 'project.manager_digest': {
      const parts = [];
      if (Number(d.due_today || 0) > 0) parts.push(`сегодня ${d.due_today}`);
      if (Number(d.overdue || 0) > 0) parts.push(`просрочено ${d.overdue}`);
      if (Number(d.incomplete || 0) > 0) parts.push(`неполных ${d.incomplete}`);
      // Пустая сводка — не сводка: молчим вовсе.
      if (!parts.length) return null;
      return `📋 Сводка по проекту · ${title}\nВыкладки: ${parts.join(' · ')}` + link;
    }
    case 'project.project_plan_ending': {
      const days = Number(d.days_left || 0);
      const open = Number(d.open_left || 0);
      return `📆 Выкладки заканчиваются · ${title}\n` +
        (days > 0 ? `Последняя дата через ${days} дн.` : 'Дат больше нет') +
        (open > 0 ? ` · не сдано ${open}` : '') +
        `\nСогласуйте следующий месяц и проставьте даты` + link;
    }
    case 'project.period_closed':
      return `🧾 Период подытожен · ${title}` +
        (Number(d.period_seq || 0) > 0 ? `\nПериод ${d.period_seq}` : '') +
        (d.starts_on && d.ends_on ? ` · ${d.starts_on} — ${d.ends_on}` : '') +
        `\nРоликов: ${Number(d.videos || 0)} · просмотров: ${num(d.views)}` +
        `\nК оплате клиенту: ${rubles(d.total)}` +
        (d.snapshot_approx ? `\n⚠️ Данные приблизительные: поденной статистики за период уже нет` : '') +
        link;
    case 'project.client_month_request':
      return `📝 Просят следующий месяц · ${title}\n${d.client_name || 'Заказчик'}` +
        (d.month ? ` · ${String(d.month).slice(0, 7)}` : '') +
        `\nРоликов: ${Number(d.videos || 0)} · креаторов: ${Number(d.creators || 0)}` +
        `\nПоказали потолок: ${rubles(d.ceiling)}` +
        `\nНужно связаться и завести заказ` + link;
    case 'order.submitted': {
      // Две ветки воронки — два разных разговора. У проекта без
      // креаторов состав собирать не надо, и путать их нельзя:
      // менеджер пойдёт искать людей, которых не будет.
      const noCrew = d.project_kind === 'brand_turnkey';
      return (noCrew ? `🎬 Заявка: видео под ключ · ${title}` : `🆕 Заявка под ключ · ${title}`) +
        `\n${client}` + (d.start_month ? ` · ${String(d.start_month).slice(0, 7)}` : '') +
        `\nРоликов: ${Number(d.videos_count || 0)}` +
        (noCrew
          ? ' · снимаем сами, креаторов нет'
          : Number(d.preferred || 0) > 0
            ? ` · отметили креаторов: ${d.preferred}`
            : ' · креаторов не отмечали') +
        (d.ceiling ? `\nПоказали потолок: ${rubles(d.ceiling)}` : '') +
        (d.brief ? `\n\n${String(d.brief).slice(0, 600)}` : '') +
        `\nПроект уже заведён — позвоните и посчитайте` + link;
    }
    case 'order.need_more':
      return `🙋 Заказ некем закрыть · заявка ${short(d.order_id)}` +
        (Number(d.need_more || 0) > 0 ? `\nНе хватает: ${d.need_more}` : '') +
        `\nРезерв кончился — добрать креаторов вручную` + link;
    case 'order.candidate_silent':
      return `🔕 Креатор не отвечает сутки · заявка ${short(d.order_id)}` +
        ` · креатор ${short(d.creator_id)}` +
        `\nМесто занято и не двигается — позвать другого` + link;
    case 'moderation.specialist_pending': {
      const why = d.reason === 'content_changed'
        ? 'изменения после одобрения'
        : 'первая публикация / повторная попытка';
      const who = d.display_name || '—';
      const modLink = d.user_id && app ? `\n${app}/admin/moderation/${d.user_id}` : '';
      return `🔍 Новая заявка на модерацию · ${who}` +
        (d.email ? `\n${d.email}` : '') + `\nПричина: ${why}` + modLink;
    }
    case 'bot.blocked':
      return `🔕 Человек заблокировал бота · ${d.bot === 'client' ? 'заказчик' : 'креатор'}` +
        `\nУведомления ему больше не уходят — если он их ждёт, скажите об этом при следующем разговоре.`;
    default:
      // Неизвестный тип: писать в чат «project.foo» нельзя, а
      // ретраить нечего — текста не появится.
      return null;
  }
}
