package projects

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Охранные тесты матрицы видов, по образцу eventroute/chat_test.go.
//
// Там таблица назначения не даёт завести событие, о котором никто не
// решил, идёт оно в чат или нет. Здесь ровно то же с видом проекта:
// каждое место, где код ветвится по виду, обязано быть названо и
// объяснено. Без этого четвёртый вид — это полтора десятка мест, которые
// надо помнить, и шестнадцатое, о котором забудут; забытое место значит,
// что у проекта без креаторов молча не идёт сбор или молча не считается
// прогресс, — а молча не работающее живёт месяцами (см. CLAUDE.md,
// историю с четырьмя красными тестами).

// kindDecision — что решили про каждый вид в одном конкретном месте.
type kindDecision struct {
	// Kinds — виды, которые проходят этот предикат. Перечислены явно, в
	// том числе когда список очевиден: очевидность и есть то, что
	// теряется при добавлении вида.
	Kinds []ProjectKind
	// Why — почему именно эти. Одной фразой, объясняющей выбор, а не
	// повторяющей SQL.
	Why string
}

// kindGatedSites — все места, где SQL спрашивает вид проекта.
//
// Ключ — «файл: предикат». Именно предикат, а не номер строки: строки
// ездят от любой правки выше, и тест начал бы падать на пустом месте.
var kindGatedSites = map[string]kindDecision{
	"internal/publications/reminders.go: pr.kind IN ('creators_turnkey', 'brand_turnkey')": {
		Kinds: []ProjectKind{KindCreatorsTurnkey, KindBrandTurnkey},
		Why: "Оба вида с планом выкладок. У проекта без креаторов просрочка такая же " +
			"настоящая, просто писать о ней некому лично: поштучные письма он не " +
			"порождает, а строки складываются в дневную сводку в общий чат менеджеров.",
	},
	"internal/publications/plan_ending.go: pr.kind IN ('creators_turnkey', 'brand_turnkey')": {
		Kinds: []ProjectKind{KindCreatorsTurnkey, KindBrandTurnkey},
		Why: "Адресат этого предупреждения и так менеджер (SendPlanEnding), " +
			"а кончившийся план значит одно и то же у обоих видов.",
	},
	"internal/publications/metrics.go: pr.kind IN ('creators_turnkey', 'brand_turnkey')": {
		Kinds: []ProjectKind{KindCreatorsTurnkey, KindBrandTurnkey},
		Why:   "Gauge «просроченных выкладок» иначе врёт: часть просрочек в него не попадёт.",
	},
	"internal/publications/weak_video.go: pr.kind IN ('creators_turnkey', 'brand_turnkey')": {
		Kinds: []ProjectKind{KindCreatorsTurnkey, KindBrandTurnkey},
		Why: "«Ролик не пошёл» и «битая ссылка» — про ролик, а не про человека. " +
			"Оба события CRMOnly, так что включение не добавляет шума в чат.",
	},
	"internal/publications/report.go: pr.kind = 'brand_turnkey'": {
		Kinds: []ProjectKind{KindBrandTurnkey},
		Why: "Стоимость проекта и СПВ по ней. У видов с креаторами деньги считаются " +
			"по начислениям (billing), и вторая сумма в отчёте разъехалась бы с первой.",
	},
	"internal/billing/tariff_registry.go: pr.kind = 'creators_turnkey'": {
		Kinds: []ProjectKind{KindCreatorsTurnkey},
		Why:   "Реестр тарифов /admin/tariff. У бренда под ключ начислений нет — в реестре ему нечего показывать.",
	},
	"internal/billing/period_confirm.go: pr.kind = 'creators_turnkey'": {
		Kinds: []ProjectKind{KindCreatorsTurnkey},
		Why:   "Подтверждение конца периода. Периодов и начислений у бренда под ключ нет.",
	},
	"internal/projects/general_repo.go: p.kind = 'general'": {
		Kinds: []ProjectKind{KindGeneral},
		Why:   "Ветка общего проекта: один исполнитель и один срок. К видам с выкладками отношения не имеет.",
	},
	"internal/projects/general_repo.go: kind = 'general'": {
		Kinds: []ProjectKind{KindGeneral},
		Why:   "Там же: правки общего проекта по id.",
	},
	"internal/projects/comments_repo.go: p.kind = 'general'": {
		Kinds: []ProjectKind{KindGeneral},
		Why: "В общем проекте клиентская ветка переписки идёт исполнителю, а не менеджеру: " +
			"менеджера у него может не быть вовсе.",
	},
}

// kindSQLPredicate — предикат по виду проекта внутри SQL-строки.
//
// Слово kind в базе носят полдюжины разных перечислений (материалы,
// портфолио, платежи, пользователи), поэтому ловим не по имени колонки, а
// по ЗНАЧЕНИЯМ: они бывают только у project_kind.
var kindSQLPredicate = regexp.MustCompile(
	`(?:[a-zA-Z_][a-zA-Z0-9_]*\.)?kind(?:::text)?\s*(?:=|<>|!=|IN)\s*\(?\s*` +
		`'(?:creators_turnkey|production_turnkey|general|brand_turnkey)'` +
		`(?:\s*,\s*'(?:creators_turnkey|production_turnkey|general|brand_turnkey)')*\s*\)?`)

// Каждое место, где SQL спрашивает вид проекта, названо и объяснено.
func TestEveryKindGatedQueryIsDecided(t *testing.T) {
	found := scanKindPredicates(t)

	if len(found) < 5 {
		t.Fatalf("нашлось всего %d предикатов по виду проекта — сломался разбор: %v",
			len(found), sortedKindKeys(found))
	}

	known := make(map[ProjectKind]bool, len(features))
	for _, k := range AllKinds() {
		known[k] = true
	}

	for _, key := range sortedKindKeys(found) {
		d, ok := kindGatedSites[key]
		if !ok {
			t.Errorf("место %q ветвится по виду проекта, а решения по нему нет — "+
				"перечислите в kindGatedSites виды, которые сюда попадают, и почему", key)
			continue
		}
		if len(d.Kinds) == 0 {
			t.Errorf("%q: список видов пуст — предикат, который не пропускает никого, "+
				"это выключенная ветка, и писать её надо не через вид", key)
		}
		if strings.TrimSpace(d.Why) == "" {
			t.Errorf("%q: решение без объяснения. Через полгода его прочитают как случайность", key)
		}
		for _, k := range d.Kinds {
			if !known[k] {
				t.Errorf("%q: вид %q не из матрицы — опечатка либо вид удалили", key, k)
			}
		}
		// Решение обязано совпадать с тем, что реально написано в SQL:
		// разъехавшийся комментарий хуже отсутствующего.
		for _, k := range AllKinds() {
			inSQL := strings.Contains(key, "'"+string(k)+"'")
			inDecision := false
			for _, kk := range d.Kinds {
				if kk == k {
					inDecision = true
				}
			}
			if inSQL != inDecision {
				t.Errorf("%q: про %q в SQL написано одно, в kindGatedSites другое", key, k)
			}
		}
	}

	// И обратно: в карте нет мест, которых в коде больше нет.
	for key := range kindGatedSites {
		if _, ok := found[key]; !ok {
			t.Errorf("kindGatedSites описывает %q, но такого предиката в коде больше нет — "+
				"список устарел", key)
		}
	}
}

// Вид проекта в Go сравнивают через матрицу, а не со строкой.
//
// Россыпь `kind == "creators_turnkey"` и была причиной, по которой
// добавление вида означало обход полутора десятков файлов. Новое
// сравнение обязано либо спрашивать FeaturesOf, либо попасть в
// kindComparisonAllowed с объяснением.
func TestKindIsComparedThroughFeatures(t *testing.T) {
	// Места, где сравнение с конкретным видом — это и есть предмет, а не
	// ветвление по возможностям.
	kindComparisonAllowed := map[string]string{
		"internal/projects/kinds.go":   "сама матрица",
		"internal/projects/dto.go":     "объявление констант",
		"internal/projects/repo.go":    "умолчание вида при создании проекта",
		"internal/projects/service.go": "умолчание вида при создании проекта",
		"internal/projects/comments_bot.go": "кто читает ветку заказчика: у общего проекта " +
			"менеджера нет, её читают заказчик и исполнитель",
	}

	root := filepath.Join("..", "..")
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
		rel := relPath(root, path)
		if _, ok := kindComparisonAllowed[rel]; ok {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				if name := kindConstName(side); name != "" {
					t.Errorf("%s: сравнение вида проекта с %s напрямую — "+
						"спросите projects.FeaturesOf(kind) о нужной возможности "+
						"либо объясните исключение в kindComparisonAllowed", rel, name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("обход исходников: %v", err)
	}
}

// Матрица покрывает каждый вид, а виды — всю матрицу.
func TestFeaturesCoverEveryKind(t *testing.T) {
	if len(AllKinds()) != len(features) {
		t.Fatalf("AllKinds перечисляет %d видов, а матрица знает %d — "+
			"новый вид завели в одном месте из двух", len(AllKinds()), len(features))
	}
	seen := make(map[ProjectKind]bool, len(features))
	for _, k := range AllKinds() {
		if !IsKnownKind(k) {
			t.Errorf("вид %q есть в AllKinds, но не в матрице", k)
		}
		if seen[k] {
			t.Errorf("вид %q перечислен в AllKinds дважды", k)
		}
		seen[k] = true
	}
	// Вид без выкладок не может иметь того, что вокруг них построено:
	// такая строка матрицы означала бы блок, которому нечего показывать.
	for kind, f := range features {
		if f.HasPublications {
			continue
		}
		for name, on := range map[string]bool{
			"HasCrew":       f.HasCrew,
			"HasReview":     f.HasReview,
			"HasChecklist":  f.HasChecklist,
			"HasBilling":    f.HasBilling,
			"HasManualCost": f.HasManualCost,
		} {
			if on {
				t.Errorf("у вида %q нет выкладок, но включён %s — этому блоку нечего показывать", kind, name)
			}
		}
		if f.PingTarget != PingNobody {
			t.Errorf("у вида %q нет выкладок, но задан адресат автопингов %q — напоминать не о чем",
				kind, f.PingTarget)
		}
	}
	// Начисления по людям и «стоимость числом» — два ответа на один
	// вопрос «сколько стоит проект». Вместе они обязаны не встречаться.
	for kind, f := range features {
		if f.HasBilling && f.HasManualCost {
			t.Errorf("у вида %q сумма считается и по начислениям, и вводится рукой — "+
				"два разных ответа на вопрос «сколько стоит проект»", kind)
		}
		if f.HasCrew && !f.HasBilling {
			t.Errorf("у вида %q есть состав, но нет начислений — людям в проекте нечем платить", kind)
		}
	}
}

// ---- разбор исходников ----

func scanKindPredicates(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join("..", "..")
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
		rel := relPath(root, path)
		// Только строковые литералы: в комментариях предикаты тоже
		// встречаются, но там они объяснение, а не поведение.
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				text = lit.Value
			}
			for _, m := range kindSQLPredicate.FindAllString(text, -1) {
				out[rel+": "+normalizeSpace(m)] = rel
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("обход исходников: %v", err)
	}
	return out
}

// kindConstName — выражение ссылается на константу вида проекта.
func kindConstName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		if strings.HasPrefix(v.Name, "Kind") && isKindConstName(v.Name) {
			return v.Name
		}
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && isKindConstName(v.Sel.Name) {
			return pkg.Name + "." + v.Sel.Name
		}
	}
	return ""
}

func isKindConstName(name string) bool {
	for _, k := range []string{
		"KindCreatorsTurnkey", "KindProductionTurnkey", "KindGeneral", "KindBrandTurnkey",
	} {
		if name == k {
			return true
		}
	}
	return false
}

func relPath(root, path string) string {
	return filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func sortedKindKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
