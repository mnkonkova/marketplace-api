package projects

import "strings"

// Матрица возможностей вида проекта.
//
// До неё вид проверялся россыпью: восемь мест сравнивали строку
// `kind == "creators_turnkey"`, ещё семь — тот же литерал внутри SQL. Пока
// видов-с-выкладками был ровно один, россыпь работала; с появлением
// четвёртого каждое забытое место означает молчаливую поломку — у проекта
// без креаторов не идут пинги или не считается прогресс, и никто об этом
// не узнаёт, потому что ошибки нет, есть пустой экран.
//
// Поэтому «что у вида есть» объявлено одним местом, а охранный тест
// (kinds_test.go) не даёт завести новую развилку по виду, не приняв по
// ней решения вслух.

// PingTarget — кому уходят автопинги по выкладкам этого вида.
type PingTarget string

const (
	// PingNobody — пингов нет: выкладок у вида нет вовсе.
	PingNobody PingTarget = ""
	// PingCreator — поштучные письма креатору в его бот.
	PingCreator PingTarget = "creator"
	// PingManagersChat — дневная сводка в общий чат менеджеров. Своего
	// бота у менеджера нет, и заводить второй канал владелец отказался.
	PingManagersChat PingTarget = "managers_chat"
)

// KindFeatures — что у вида проекта есть, а чего нет.
//
// Поля отвечают на вопрос «работает ли блок», а не «показывать ли его»:
// фронтовая карта блоков (project-blocks.ts) решает второе и опирается
// на первое.
type KindFeatures struct {
	// HasFunnel — воронка pipelines: стадии, шаги, ответственный за шаг.
	HasFunnel bool
	// HasPublications — план выкладок, площадки, ежедневный сбор,
	// статистика, отчёт и календарь.
	HasPublications bool
	// HasCrew — состав проекта (project_creators). Без него нет ни
	// ростера, ни приглашений, ни отчёта «по креаторам».
	HasCrew bool
	// HasReview — проверка ролика менеджером (publication_reviews).
	HasReview bool
	// HasChecklist — чек-лист требований к выкладке.
	HasChecklist bool
	// HasBilling — начисления по людям: периоды, ступени, выплаты.
	HasBilling bool
	// HasAccounts — аккаунты, с которых выходят ролики.
	HasAccounts bool
	// HasMaterials — материалы проекта.
	HasMaterials bool
	// HasManualCost — стоимость проекта за период вводит менеджер рукой.
	//
	// У вида с креаторами сумма складывается из начислений людям, и
	// СПВ считается по ней. Там, где людей нет, складывать нечего —
	// сумму называет менеджер, и она же идёт в делимое СПВ.
	HasManualCost bool
	// PingTarget — кому уходят напоминания по выкладкам.
	PingTarget PingTarget
}

// features — таблица. Ключ обязан покрывать AllKinds целиком: за этим
// следит TestFeaturesCoverEveryKind.
var features = map[ProjectKind]KindFeatures{
	KindCreatorsTurnkey: {
		HasPublications: true,
		HasCrew:         true,
		HasReview:       true,
		HasChecklist:    true,
		HasBilling:      true,
		HasAccounts:     true,
		HasMaterials:    true,
		PingTarget:      PingCreator,
	},
	// Продакшн под ключ — это воронка и ничего кроме: выкладок у него нет
	// (см. assertProjectHasPublications), и всё, что вокруг выкладок, для
	// него не существует.
	KindProductionTurnkey: {
		HasFunnel:  true,
		PingTarget: PingNobody,
	},
	// Общий проект — один исполнитель и один срок. Ни воронки, ни выкладок.
	KindGeneral: {
		PingTarget: PingNobody,
	},
	// Бренд под ключ — это креаторы под ключ минус люди. Ролики выходят с
	// аккаунтов бренда: поручать выкладку некому, проверять работу не у
	// кого, начислять нечего и некому. Остальное — план, площадки, сбор,
	// отчёт, аккаунты, материалы — работает ровно так же, потому что сбор
	// ходит по publication_links и про креаторов не знает.
	KindBrandTurnkey: {
		HasPublications: true,
		HasAccounts:     true,
		HasMaterials:    true,
		HasManualCost:   true,
		PingTarget:      PingManagersChat,
	},
}

// FeaturesOf — что умеет вид проекта.
//
// Неизвестный вид получает пустую матрицу, а не панику: вид приезжает из
// базы, и старая строка с видом, которого в коде уже нет, не повод ронять
// весь список проектов. Пустая матрица значит «ничего не включаем» —
// безопасное умолчание: лишний выключенный блок видно, лишний включённый
// падает на пустых данных.
func FeaturesOf(kind ProjectKind) KindFeatures { return features[kind] }

// AllKinds — виды в порядке, в котором их показывают человеку.
//
// Порядок зафиксирован здесь, а не собирается из map: по нему строятся и
// подсказка в отказе фильтра, и нули в админской сводке, и обход в
// тестах — а обход map в Go случайный.
func AllKinds() []ProjectKind {
	return []ProjectKind{
		KindCreatorsTurnkey,
		KindBrandTurnkey,
		KindProductionTurnkey,
		KindGeneral,
	}
}

// IsKnownKind — вид из enum project_kind.
//
// Проверяем в Go, а не приведением к типу в SQL: незнакомое значение
// иначе доходит до базы и возвращается ошибкой enum'а, то есть пятисотой
// вместо внятного отказа.
func IsKnownKind(kind ProjectKind) bool {
	_, ok := features[kind]
	return ok
}

// KindsHint — перечень видов для текста отказа. Фильтры живут в адресе и
// ссылками делятся: устаревшая ссылка не должна ронять экран, а отказ
// обязан говорить, что именно принято.
func KindsHint() string {
	names := make([]string, 0, len(features))
	for _, k := range AllKinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}
