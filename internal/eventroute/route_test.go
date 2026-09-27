package eventroute

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/notifications"
	"marketpclce/internal/outbox"
)

// recorder — диспатчер, который ничего не шлёт, а запоминает. Настоящие
// обработчики без него проверить нельзя: адрес вебхука виден только по
// тому, в какой из трёх диспатчеров ушло событие.
type recorder struct{ sent []notifications.Payload }

func (r *recorder) Send(_ context.Context, p notifications.Payload) error {
	r.sent = append(r.sent, p)
	return nil
}

type stubIndexer struct{ reconciled, deleted []uuid.UUID }

func (s *stubIndexer) Reconcile(_ context.Context, id uuid.UUID, _ int64) error {
	s.reconciled = append(s.reconciled, id)
	return nil
}
func (s *stubIndexer) Delete(_ context.Context, id uuid.UUID, _ int64) error {
	s.deleted = append(s.deleted, id)
	return nil
}

type stubFeed struct{ reconciled, deleted []uuid.UUID }

func (s *stubFeed) ReconcileVideos(_ context.Context, id uuid.UUID) error {
	s.reconciled = append(s.reconciled, id)
	return nil
}
func (s *stubFeed) DeleteByUser(_ context.Context, id uuid.UUID) error {
	s.deleted = append(s.deleted, id)
	return nil
}

// Каждый агрегат уходит в тот вебхук, который ему полагается.
//
// Раньше проверить это было нечем: три анонимных замыкания в main()
// отличались одной строкой, и перепутанные местами адреса — CRM-события
// в почтовый вебхук — не поймал бы никто. Ошибки бы не было: n8n
// отвечает 200 на что угодно.
func TestEachAggregateGoesToItsOwnWebhook(t *testing.T) {
	crm, email, support := &recorder{}, &recorder{}, &recorder{}
	handlers := Handlers(Deps{CRM: crm, Email: email, Support: support})

	cases := []struct {
		aggregate string
		eventType string
		want      *recorder
		otherOne  *recorder
		otherTwo  *recorder
	}{
		{outbox.AggregateProject, "project.created", crm, email, support},
		{outbox.AggregateModeration, outbox.EventModerationSpecialistPending, crm, email, support},
		{outbox.AggregateSupport, outbox.EventSupportMessageReceived, support, crm, email},
		{outbox.AggregateEmail, outbox.EventEmailVerifySend, email, crm, support},
	}
	for _, c := range cases {
		t.Run(c.aggregate, func(t *testing.T) {
			crm.sent, email.sent, support.sent = nil, nil, nil
			h, ok := handlers[c.aggregate]
			if !ok {
				t.Fatalf("нет обработчика для агрегата %q", c.aggregate)
			}
			if err := h(context.Background(), 42, uuid.NewString(), c.eventType, []byte(`{}`)); err != nil {
				t.Fatalf("обработчик вернул ошибку: %v", err)
			}
			if len(c.want.sent) != 1 {
				t.Fatalf("событие не ушло в свой вебхук (ушло %d штук)", len(c.want.sent))
			}
			if n := len(c.otherOne.sent) + len(c.otherTwo.sent); n != 0 {
				t.Errorf("событие уехало в чужой вебхук (%d лишних отправок)", n)
			}
			if got := c.want.sent[0].Aggregate; got != c.aggregate && c.aggregate != outbox.AggregateEmail {
				t.Errorf("в теле агрегат %q, ожидали %q", got, c.aggregate)
			}
			if got := c.want.sent[0].EventType; got != c.eventType {
				t.Errorf("в теле тип %q, ожидали %q", got, c.eventType)
			}
		})
	}
}

// event_id в теле — это id строки outbox, а не что-нибудь производное.
// По нему n8n дедуплицирует: когда-то id собирали из aggregate_id и типа
// события, и два одинаковых события по одному проекту были для n8n одним.
func TestEventIDIsOutboxRowID(t *testing.T) {
	crm := &recorder{}
	h := Handlers(Deps{CRM: crm})[outbox.AggregateProject]

	const outboxID int64 = 918273
	if err := h(context.Background(), outboxID, uuid.NewString(), "project.created", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("обработчик вернул ошибку: %v", err)
	}
	if len(crm.sent) != 1 {
		t.Fatalf("отправок %d, ожидали одну", len(crm.sent))
	}
	if got := crm.sent[0].EventID; got != strconv.FormatInt(outboxID, 10) {
		t.Errorf("event_id %q, ожидали id строки outbox (%d)", got, outboxID)
	}
	if got := string(crm.sent[0].Data); got != `{"a":1}` {
		t.Errorf("payload доехал изменённым: %s", got)
	}
}

// Выключенный вебхук — не ошибка: событие квитируется, чтобы не копить
// ретраи на стенде без n8n.
func TestDisabledWebhookIsNoOp(t *testing.T) {
	handlers := Handlers(Deps{})
	for _, aggregate := range []string{
		outbox.AggregateProject, outbox.AggregateModeration, outbox.AggregateSupport,
	} {
		if err := handlers[aggregate](context.Background(), 1, uuid.NewString(), "project.created", nil); err != nil {
			t.Errorf("%s: %v", aggregate, err)
		}
	}
}

// Специалист переиндексируется и в поиске, и в ленте; удаление — тоже в
// обоих местах. Забыть половину легко: это два разных индекса.
func TestSpecialistHandlerTouchesBothIndexes(t *testing.T) {
	search, feed := &stubIndexer{}, &stubFeed{}
	h := Handlers(Deps{Search: search, Feed: feed})[outbox.AggregateSpecialist]
	id := uuid.New()

	if err := h(context.Background(), 1, id.String(), outbox.EventSpecialistUpserted, []byte(`{}`)); err != nil {
		t.Fatalf("upserted: %v", err)
	}
	if len(search.reconciled) != 1 || len(feed.reconciled) != 1 {
		t.Errorf("переиндексация задела не оба индекса: поиск %d, лента %d",
			len(search.reconciled), len(feed.reconciled))
	}
	if err := h(context.Background(), 2, id.String(), outbox.EventSpecialistDeleted, []byte(`{}`)); err != nil {
		t.Fatalf("deleted: %v", err)
	}
	if len(search.deleted) != 1 || len(feed.deleted) != 1 {
		t.Errorf("удаление задело не оба индекса: поиск %d, лента %d",
			len(search.deleted), len(feed.deleted))
	}
}

// Выключенный транскодер квитирует событие, а незнакомый тип в
// портфолио — перманентная ошибка: ретраить его бессмысленно.
func TestPortfolioHandler(t *testing.T) {
	if err := Handlers(Deps{})[outbox.AggregatePortfolio](
		context.Background(), 1, uuid.NewString(), outbox.EventPortfolioVideoUploaded, nil); err != nil {
		t.Errorf("выключенный транскодер вернул ошибку: %v", err)
	}
	h := Handlers(Deps{Transcoder: okTranscoder{}})[outbox.AggregatePortfolio]
	err := h(context.Background(), 1, uuid.NewString(), "portfolio.unknown", nil)
	if !errors.Is(err, outbox.ErrPermanent) {
		t.Errorf("незнакомый тип: %v, ожидали перманентную ошибку", err)
	}
}

type okTranscoder struct{}

func (okTranscoder) Process(context.Context, []byte) error { return nil }

// Таблица обработчиков покрывает все агрегаты, объявленные в
// internal/outbox/emit.go.
//
// Агрегат без обработчика воркер молча помечает обработанным: одна
// строка warn в логе — и событие потеряно навсегда, без ошибки и без
// метрики. Список агрегатов читаем из исходника, а не переписываем сюда
// руками: переписанный список устаревает ровно тогда, когда он нужен —
// в момент, когда кто-то завёл новый агрегат.
func TestHandlersCoverEveryAggregate(t *testing.T) {
	declared := constantsWithPrefix(t, "../outbox/emit.go", "Aggregate")
	if len(declared) < 5 {
		t.Fatalf("в emit.go нашлось всего %d агрегатов — сломался разбор исходника: %v",
			len(declared), declared)
	}
	handlers := Handlers(Deps{})
	for name, value := range declared {
		if _, ok := handlers[value]; !ok {
			t.Errorf("агрегат %s (%q) объявлен в outbox, но обработчика для него нет — "+
				"его события будут молча пропадать", name, value)
		}
	}
	for value := range handlers {
		found := false
		for _, declaredValue := range declared {
			if declaredValue == value {
				found = true
			}
		}
		if !found {
			t.Errorf("обработчик для агрегата %q, которого нет в outbox", value)
		}
	}
}

// constantsWithPrefix — значения строковых констант с данным префиксом
// имени из одного файла.
func constantsWithPrefix(t *testing.T, path, prefix string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("разобрать %s: %v", path, err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, prefix) || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			out[name.Name] = v
		}
		return true
	})
	return out
}
