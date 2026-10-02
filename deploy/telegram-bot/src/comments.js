// Ответ боту → комментарий в проекте: что бот делает с сообщением
// человека и с кнопкой «Написать в проект».
//
// Вынесено из server.js, чтобы проверять скриптом без сети: зависимости
// (API, Telegram, лог) приходят снаружи. Правило одно и держит его API:
// комментарием становится только ответ на сообщение бота о проекте,
// которое API запомнил. Простой текст в API не уходит — подсказка.
//
// Сеть падает: API может не ответить вовсе или ответить 5xx. Человек при
// этом должен услышать «не получилось», а кнопка — перестать крутить
// «часики»: молчание бота читается как «записала».

import * as texts from './texts.js';

const errText = (err) => String(err?.message || err).slice(0, 300);

/**
 * deps: { api: { comment, commentAnchor, whoIs },
 *         sendMessage(bot, chatID, text, extra),
 *         answerCallbackQuery(bot, callbackID, text, showAlert),
 *         log(level, msg, extra) }
 */
export function createCommentHandlers({ api, sendMessage, answerCallbackQuery, log = () => {} }) {
  const notLinkedText = (bot) => `${texts.help[bot]}\n\n${texts.notLinked}`;

  /** Ответ (reply) в личке — в API: он найдёт проект или откажет. */
  async function onComment(bot, u) {
    let res;
    try {
      res = await api.comment(bot, u);
    } catch (err) {
      // Сеть, таймаут: комментарий не записан, и человек должен это
      // знать — иначе он решит, что менеджер уже прочитал.
      log('warn', 'comment failed', { bot, err: errText(err) });
      await sendMessage(bot, u.chatID, texts.commentRefused.failed);
      return;
    }
    if (res.status === 201) {
      const { project_id: projectID, project_title: title, reader } = res.data || {};
      const sent = await sendMessage(bot, u.chatID, texts.commentSaved(title, reader));
      // Ответ на подтверждение — продолжение того же разговора: просим
      // API запомнить и его. Не вышло — ответ на подтверждение получит
      // «не поняла, к какому проекту», но сам комментарий уже записан.
      if (sent?.message_id && projectID) {
        await api
          .commentAnchor(bot, { ...u, projectID, messageID: sent.message_id })
          .catch((err) => log('warn', 'saved reply not anchored', { bot, err: errText(err) }));
      }
      log('info', 'comment saved', { bot });
      return;
    }
    const reason = res.data?.error || '';
    const text =
      reason === 'not_linked'
        ? notLinkedText(bot)
        : texts.commentRefused[reason] || texts.commentRefused.failed;
    await sendMessage(bot, u.chatID, text);
    log('warn', 'comment refused', { bot, status: res.status, reason });
  }

  /**
   * Простой текст, не ответ ни на что. В проект не пишем и в API как
   * комментарий не отправляем — только подсказка. Привязку спрашиваем,
   * чтобы непривязанному не советовать кнопку, которой у него нет; API
   * недоступен — подсказка всё равно уместнее справки.
   */
  async function onPlain(bot, u) {
    let linked = true;
    try {
      const who = await api.whoIs(bot, u.tgUserID);
      linked = who.status !== 404;
    } catch (err) {
      log('warn', 'whois failed', { bot, err: errText(err) });
    }
    await sendMessage(bot, u.chatID, linked ? texts.writeHint : notLinkedText(bot));
  }

  /**
   * Кнопка «Написать в проект»: спросить API, можно ли, отправить
   * приглашение с force_reply и попросить API запомнить его — ответ на
   * приглашение уйдёт в этот проект тем же путём, что ответ на пинг.
   *
   * На нажатие отвечаем ВСЕГДА, ровно один раз и что бы ни случилось:
   * иначе Telegram полминуты крутит на кнопке «часики», и человек жмёт
   * её снова.
   */
  async function onWriteButton(bot, u) {
    let answered = false;
    const answer = (text = '', alert = false) => {
      answered = true;
      return answerCallbackQuery(bot, u.callbackID, text.slice(0, 190), alert).catch(() => {});
    };
    try {
      let check;
      try {
        check = await api.commentAnchor(bot, u);
      } catch (err) {
        log('warn', 'write button failed', { bot, err: errText(err) });
        await answer(texts.commentRefused.failed, true);
        return;
      }
      if (check.status !== 200) {
        const reason = check.data?.error || '';
        const text =
          reason === 'not_linked'
            ? texts.notLinked
            : texts.commentRefused[reason] || texts.commentRefused.failed;
        await answer(text, true);
        log('warn', 'write button refused', { bot, status: check.status, reason });
        return;
      }
      await answer();
      const prompt = await sendMessage(bot, u.chatID, texts.writePrompt(check.data?.project_title), {
        reply_markup: { force_reply: true, input_field_placeholder: 'Комментарий в проект' },
      });
      let reg;
      try {
        reg = await api.commentAnchor(bot, { ...u, messageID: prompt?.message_id });
      } catch (err) {
        reg = { status: 0, err: errText(err) };
      }
      if (reg.status !== 200) {
        // Приглашение уже у человека, а API его не запомнил: ответ на
        // него получит отказ. Честнее сказать сразу, чем после того, как
        // человек напишет длинный текст.
        log('warn', 'write prompt not anchored', { bot, status: reg.status, err: reg.err });
        await sendMessage(bot, u.chatID, texts.writePromptLost);
      }
    } finally {
      if (!answered) await answer();
    }
  }

  return { onComment, onPlain, onWriteButton };
}
