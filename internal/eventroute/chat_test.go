package eventroute

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Тесты, ради которых заведена таблица назначения.
//
// Оба падают на молчаливой потере события: первый — когда тип завели, а
// решение «в чат или нет» не приняли; второй — когда решение приняли, а
// workflow про него не знает и выбрасывает событие в ветку default,
// ответив нам 200.

// crmWebhookPrefixes — типы, уезжающие в общий CRM-вебхук. support.*
// ходит своим адресом и своим workflow (crmSupport), поэтому в сверке с
// crmTgEventsV1 не участвует.
var crmWebhookPrefixes = []string{"project.", "order.", "moderation."}

// emitArgIndex — где в вызове лежит тип события. Три способа положить
// событие в outbox: общий outbox.Emit и две обёртки в projects, которые
// сами подставляют агрегат.
var emitArgIndex = map[string]int{
	"Emit":        4, // outbox.Emit(ctx, tx, aggregate, aggregateID, eventType, payload)
	"emit":        5, // projects.emit(ctx, tx, projectID, stepID, actorID, eventType, payload)
	"emitGeneral": 4, // projects.emitGeneral(ctx, tx, projectID, actorID, eventType, payload)
}

// dynamicEmitSites — места, где тип события собирается на лету
// ("project." + kind) и из исходника литералом не читается.
//
// Список явный, потому что иначе такие события — а это вся сводка
// менеджерам и все напоминания о выкладках — невидимы для проверки:
// именно они и терялись. Новое такое место обязано попасть сюда вместе
// со списком типов, которые оно порождает.
var dynamicEmitSites = map[string][]string{
	"internal/projects/manager_repo.go": {
		// "project." + eventKind, где eventKind = assigned | unassigned.
		"project.assigned",
		"project.unassigned",
	},
	"internal/projects/general_repo.go": {
		// event = rework | accepted, выбирается по решению клиента.
		"project.general_rework",
		"project.general_accepted",
	},
	"internal/publications/reminders.go": {
		// "project." + Reminder*-константа.
		"project.publication_due_tomorrow",
		"project.publication_due_today",
		"project.publication_overdue",
		"project.publication_incomplete",
		"project.publication_manual",
		"project.manager_digest",
		// Клиентские уведомления идут тем же Repo.Send: порог
		// просмотров, новый ролик и сдвиг даты.
		"project.client_views_threshold",
		"project.client_new_video",
		"project.client_date_shift",
	},
	"internal/publications/client_notify.go": {
		// "project." + kind недельной сводки заказчику.
		"project.client_weekly_digest",
	},
	"internal/publications/weak_video.go": {
		// "project." + WeakVideo.Kind(): мало просмотров или битая ссылка.
		"project.publication_weak",
		"project.publication_dead_link",
	},
	"internal/publications/brief.go": {
		// "project." + Reminder*-константа задания креатора.
		"project.project_materials_updated",
		"project.project_checklist_updated",
		"project.project_creator_briefed",
	},
	"internal/publications/plan_ending.go": {
		// "project." + ReminderPlanEnding.
		"project.project_plan_ending",
	},
	"internal/publications/review.go": {
		// event = accepted | returned, выбирается по решению менеджера.
		"project.publication_accepted",
		"project.publication_returned",
	},
	"internal/orders/repo.go": {
		// event = accepted | declined, выбирается по ответу креатора.
		"order.invitation_accepted",
		"order.invitation_declined",
	},
}

// Каждый тип события, который код умеет положить в CRM-вебхук или в
// поддержку, объявлен в таблице назначения.
//
// Появился новый тип — тест падает, и человек обязан выбрать: идёт он в
// чат или намеренно нет. Ровно этого выбора до сих пор никто не делал —
// события просто добавляли, а workflow о них не знал.
func TestEveryEmittedEventTypeIsRouted(t *testing.T) {
	literal, dynamic := scanEmittedEventTypes(t)

	if len(literal) < 20 {
		t.Fatalf("в исходниках нашлось всего %d типов событий — сломался разбор: %v",
			len(literal), sortedKeys(literal))
	}

	for eventType, where := range literal {
		if _, ok := DeliveryOf(eventType); !ok {
			t.Errorf("тип %q (%s) не объявлен в таблице назначения — "+
				"решите, идёт он в чат или нет", eventType, where)
		}
	}

	// Места с динамическим типом события: их не прочитать литералом,
	// поэтому требуем, чтобы каждое было описано в dynamicEmitSites.
	for _, where := range sortedKeys(dynamic) {
		if _, ok := dynamicEmitSites[where]; !ok {
			t.Errorf("в %s тип события собирается на лету, а в dynamicEmitSites его нет: "+
				"перечислите типы, которые там рождаются, иначе они невидимы для проверки", where)
		}
	}
	for where, types := range dynamicEmitSites {
		if _, ok := dynamic[where]; !ok {
			t.Errorf("dynamicEmitSites описывает %s, но такого места в коде больше нет — "+
				"список устарел", where)
		}
		for _, eventType := range types {
			if _, ok := DeliveryOf(eventType); !ok {
				t.Errorf("тип %q (%s, собирается на лету) не объявлен в таблице назначения",
					eventType, where)
			}
		}
	}

	// И обратно: в таблице нет типов, которых код не порождает. Иначе в
	// ней копится то, чего давно нет, и workflow рендерит мёртвые ветки.
	for _, eventType := range KnownEvents() {
		if _, ok := literal[eventType]; ok {
			continue
		}
		if inDynamicSites(eventType) {
			continue
		}
		t.Errorf("тип %q объявлен в таблице, но код его не emit'ит — таблица устарела", eventType)
	}
}

// Набор типов, который разбирает workflow, совпадает с «идёт в чат».
//
// Это и есть тот тихий сбой, ради которого всё затевалось: workflow
// отвечает 200 на любое событие, а незнакомое выбрасывает веткой
// default. Воркер считает доставку успешной, метрики чистые — и
// напоминания о просрочках уходят в никуда.
func TestWorkflowHandlesExactlyChatEvents(t *testing.T) {
	const path = "../../deploy/n8n/workflows/crmTgEventsV1.json"
	handled := workflowEventTypes(t, path)

	want := map[string]bool{}
	for _, e := range ChatEvents() {
		if isCRMWebhook(e) {
			want[e] = true
		}
	}

	for e := range want {
		if !handled[e] {
			t.Errorf("тип %q помечен «идёт в чат», но workflow его не разбирает — "+
				"событие молча уедет в ветку default", e)
		}
	}
	for e := range handled {
		if !want[e] {
			t.Errorf("workflow разбирает %q, но в таблице он не помечен «идёт в чат» — "+
				"либо поправьте таблицу, либо уберите ветку из workflow", e)
		}
	}
}

// isCRMWebhook — событие уходит в общий CRM-вебхук (а не в поддержку).
func isCRMWebhook(eventType string) bool {
	for _, p := range crmWebhookPrefixes {
		if strings.HasPrefix(eventType, p) {
			return true
		}
	}
	return false
}

func inDynamicSites(eventType string) bool {
	for _, types := range dynamicEmitSites {
		for _, t := range types {
			if t == eventType {
				return true
			}
		}
	}
	return false
}

// scanEmittedEventTypes — что код кладёт в outbox для CRM и поддержки.
//
// Читаем исходники, а не перечисляем руками: перечисленный список
// устаревает ровно в тот момент, когда он нужен — когда кто-то добавил
// событие и не подумал про чат.
//
// Возвращает (тип события → где нашли) и (файл с динамическим типом →
// что там за вызов).
func scanEmittedEventTypes(t *testing.T) (literal map[string]string, dynamic map[string]string) {
	t.Helper()
	literal, dynamic = map[string]string{}, map[string]string{}

	root := "../.." // корень репозитория относительно пакета
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return fmt.Errorf("разобрать %s: %w", path, perr)
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call.Fun)
			idx, ok := emitArgIndex[name]
			if !ok || idx >= len(call.Args) {
				return true
			}
			// Агрегат решает, доедет ли событие до чата вообще.
			// specialist.* и portfolio.* тоже собирают тип на лету, но
			// это индексация и транскодинг — им в таблице делать нечего.
			if name == "Emit" && !isChatAggregate(t, root, call.Args[2]) {
				return true
			}
			switch arg := call.Args[idx].(type) {
			case *ast.BasicLit:
				if arg.Kind != token.STRING {
					return true
				}
				v, uerr := strconv.Unquote(arg.Value)
				if uerr != nil || !isRoutedPrefix(v) {
					return true
				}
				literal[v] = rel
			default:
				// Тип собирается на лету — литералом его не прочитать.
				// Сюда попадают и вызовы с константой из outbox
				// (outbox.EventProjectCreated): их значение читаем ниже.
				if v, ok := selectorConstValue(t, root, call.Args[idx]); ok {
					if isRoutedPrefix(v) {
						literal[v] = rel
					}
					return true
				}
				dynamic[rel] = rel
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("обойти исходники: %v", err)
	}
	return literal, dynamic
}

// isChatAggregate — событие уезжает в CRM-вебхук или в поддержку.
// Неразрешимое выражение считаем «да»: лучше лишний раз потребовать
// описать место, чем молча пропустить события мимо проверки.
func isChatAggregate(t *testing.T, root string, expr ast.Expr) bool {
	t.Helper()
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return true
	}
	if aggregateConsts == nil {
		aggregateConsts = constantsWithPrefix(t, filepath.Join(root, "internal/outbox/emit.go"), "Aggregate")
	}
	v, ok := aggregateConsts[sel.Sel.Name]
	if !ok {
		return true
	}
	return v == "project" || v == "moderation" || v == "support"
}

// outboxConsts / aggregateConsts — константы outbox, прочитанные один раз.
var (
	outboxConsts    map[string]string
	aggregateConsts map[string]string
)

// selectorConstValue — значение выражения вида outbox.EventXxx. Такие
// вызовы формально не литералы, но тип события в них вполне известен.
func selectorConstValue(t *testing.T, root string, expr ast.Expr) (string, bool) {
	t.Helper()
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if outboxConsts == nil {
		outboxConsts = constantsWithPrefix(t, filepath.Join(root, "internal/outbox/emit.go"), "Event")
	}
	v, ok := outboxConsts[sel.Sel.Name]
	return v, ok
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// isRoutedPrefix — тип события из тех, что уезжают в чат-вебхуки.
// specialist.*, email.* и portfolio.* — внутренняя механика (индексация,
// письма, транскодинг), к чату отношения не имеют.
func isRoutedPrefix(eventType string) bool {
	return isCRMWebhook(eventType) || strings.HasPrefix(eventType, "support.")
}

// workflowEventTypes — какие типы разбирает n8n-workflow. Файл лежит в
// репозитории, и читать его из теста — единственный способ заметить, что
// он разошёлся с кодом.
func workflowEventTypes(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("прочитать workflow: %v", err)
	}
	var wf struct {
		Nodes []struct {
			Type       string `json:"type"`
			Parameters struct {
				JSCode string `json:"jsCode"`
			} `json:"parameters"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("разобрать workflow: %v", err)
	}
	caseRe := regexp.MustCompile(`case\s+'([a-z_]+\.[a-z_]+)'\s*:`)
	out := map[string]bool{}
	for _, n := range wf.Nodes {
		for _, m := range caseRe.FindAllStringSubmatch(n.Parameters.JSCode, -1) {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("в workflow не нашлось ни одной ветки case — сломался разбор %s", path)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
