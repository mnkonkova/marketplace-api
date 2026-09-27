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
    'Это бот «Сотки» для креаторов.\n\n' +
    'Здесь приходят сроки выкладок, заявки от заказчиков и решения по роликам. ' +
    'Работа живёт в кабинете — бот только сообщает, что в нём появилось.',
  client:
    'Это бот «Сотки» для заказчиков.\n\n' +
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
