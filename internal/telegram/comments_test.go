package telegram

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// Квитанция старого формата обязана разбираться как раньше: бот и API
// выкатываются по отдельности, и между выкатками API получает от
// старого бота квитанцию без sent.
func TestAckRequestBackwardCompatible(t *testing.T) {
	var old botAckReq
	if err := json.Unmarshal([]byte(`{"delivered":[1,2],"failed":[{"id":3,"error":"x"}]}`), &old); err != nil {
		t.Fatalf("старый формат: %v", err)
	}
	if len(old.Delivered) != 2 || len(old.Failed) != 1 || len(old.Sent) != 0 {
		t.Errorf("старый формат разобран не так: %+v", old)
	}

	var cur botAckReq
	body := `{"delivered":[7],"failed":[],
	  "sent":[{"id":7,"chat_id":482512345,"message_id":901},{"id":7,"chat_id":5,"message_id":12}]}`
	if err := json.Unmarshal([]byte(body), &cur); err != nil {
		t.Fatalf("новый формат: %v", err)
	}
	if len(cur.Sent) != 2 {
		t.Fatalf("sent: %+v", cur.Sent)
	}
	if got := cur.Sent[0]; got.ID != 7 || got.ChatID != 482512345 || got.MessageID != 901 {
		t.Errorf("sent[0] = %+v", got)
	}
}

// Проект и человека берём из нашего конверта, а не из квитанции.
func TestSentTarget(t *testing.T) {
	pid := uuid.New()
	alice, bob := uuid.New(), uuid.New()
	env := func(audience, project string) []byte {
		b, _ := json.Marshal(map[string]any{
			"audience": audience,
			"data":     map[string]any{"project_id": project, "project_title": "П"},
			"recipients": []map[string]any{
				{"user_id": alice, "tg_chat_id": 100},
				{"user_id": bob, "tg_chat_id": 200},
			},
		})
		return b
	}

	t.Run("получатель найден по чату", func(t *testing.T) {
		gotP, gotU, ok := sentTarget(env("person", pid.String()), 200)
		if !ok || gotP != pid || gotU != bob {
			t.Errorf("got %v %v %v", gotP, gotU, ok)
		}
	})
	t.Run("чужой чат не принимаем", func(t *testing.T) {
		// Иначе ошибка на стороне бота давала бы записать соответствие
		// для того, кому мы не писали.
		if _, _, ok := sentTarget(env("person", pid.String()), 300); ok {
			t.Error("чат не из получателей принят")
		}
	})
	t.Run("чат менеджеров — не комментарии", func(t *testing.T) {
		if _, _, ok := sentTarget(env("managers", pid.String()), 100); ok {
			t.Error("сообщение в общий чат записано как личное")
		}
	})
	t.Run("без проекта нечего запоминать", func(t *testing.T) {
		if _, _, ok := sentTarget(env("person", ""), 100); ok {
			t.Error("заявка без проекта записана")
		}
		if _, _, ok := sentTarget(env("person", "не-uuid"), 100); ok {
			t.Error("битый project_id записан")
		}
	})
	t.Run("битый конверт", func(t *testing.T) {
		if _, _, ok := sentTarget([]byte("{"), 100); ok {
			t.Error("битый конверт принят")
		}
	})
}
