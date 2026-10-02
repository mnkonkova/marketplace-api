package eventroute

import "testing"

// На что можно ответить в проект. Заявку получают те, кто в проекте
// ещё не состоит, — их ответ упал бы с «вы больше не участвуете».
// Сообщения участникам проекта, наоборот, обязаны быть отвечаемыми:
// иначе ответ на пинг молча не записывается.
func TestReplyableRoutes(t *testing.T) {
	for _, ev := range []string{"order.invitation_sent", "order.broadcast_sent"} {
		if botRouting[ev].replyable {
			t.Errorf("%s: на заявку отвечают откликом, а не комментарием в проект", ev)
		}
	}
	for _, ev := range []string{
		"project.publication_due_today", "project.publication_manual",
		"project.document_delivered", "project.client_document_delivered",
		"project.client_weekly_digest",
	} {
		if r, ok := botRouting[ev]; !ok || !r.replyable {
			t.Errorf("%s: ответ участника на это сообщение должен попасть в проект", ev)
		}
	}
}
