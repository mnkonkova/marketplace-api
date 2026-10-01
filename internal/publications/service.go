package publications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	// ErrNoLinks — сдача без единой ссылки. Отдельная ошибка: это самая
	// частая опечатка на фронте, и она не должна выглядеть как поломка.
	ErrNoLinks = errors.New("не передано ни одной ссылки")
	// ErrDuplicatePlatform — две ссылки на одну площадку в одной сдаче.
	// В БД это поймает UNIQUE, но тогда одна из ссылок молча перезапишет
	// другую, и креатор не узнает, какая уехала.
	ErrDuplicatePlatform = errors.New("две ссылки на одну площадку")
	// ErrCollectorNotSet — сбор статистики не настроен (нет адреса или
	// ключа instacurl). Ошибка, а не тихий no-op: пустой отчёт читается
	// как «ролики никто не смотрит», и это худшая из возможных подмен.
	ErrCollectorNotSet = errors.New("сбор статистики не настроен")
	// ErrCollapsedNoDetail — у закрытого проекта ежедневный ряд схлопнут
	// в один снимок на весь проект, и разбивки по креаторам больше нет.
	ErrCollapsedNoDetail = errors.New("детализация по креаторам не хранится")
)

// maxBatch — потолок на одну пачку. 3 креатора × 60 дней = 180; 500 даёт
// запас и одновременно ловит случай «выбрал весь каталог».
const maxBatch = 500

// maxPerDay — сколько роликов можно поставить на один день.
//
// Не про технику: тридцать роликов в один день — это опечатка в поле
// «роликов в день», а не план съёмок, и дешевле отказать сразу, чем
// вычищать потом. То же число стоит ограничением в базе (миграция
// 00076), здесь — чтобы человек получил внятный отказ, а не ошибку
// драйвера.
const maxPerDay = 10

type Service struct {
	repo *Repo
	// collector — сбор статистики. nil, если интеграция не настроена:
	// тогда RunCollection честно возвращает ошибку, а не тихо ничего
	// не делает.
	collector Collector
	// scanner — обход аккаунтов креаторов. Отдельно от collector, потому
	// что включается отдельным ключом: сбор по сданным ссылкам
	// обязателен, а поиск новых роликов — расход сверх него.
	scanner AccountScanner
	// secrets — шифрование паролей от аккаунтов бренда. Пустой (ключа в
	// окружении нет) — логины и ссылки работают, пароли не заводятся.
	secrets *Secrets
	// expander — разворачиватель коротких ссылок «поделиться». nil —
	// короткая ссылка сохраняется как есть, как было до 1 октября 2026.
	expander URLExpander
}

func NewService(repo *Repo) *Service {
	return &Service{repo: repo, secrets: &Secrets{}}
}

// WithSecrets — включить хранение паролей. Ключ приходит из конфига, в
// базе его нет намеренно.
func (s *Service) WithSecrets(sec *Secrets) *Service {
	s.secrets = sec
	return s
}

// SecretsEnabled — можно ли заводить пароли.
func (s *Service) SecretsEnabled() bool { return s.secrets.Enabled() }

// CreateBatchByScheme — простановка дат по быстрой схеме. Это основной путь
// менеджера: выбрал креаторов, выбрал «вторник-четверг» и границы месяца.
func (s *Service) CreateBatchByScheme(ctx context.Context, projectID uuid.UUID,
	creatorIDs []uuid.UUID, scheme Scheme, from, to time.Time,
	draftLeadDays, perDay int, createdBy uuid.UUID) (BatchResult, error) {

	dates, err := GenerateDates(scheme, from, to)
	if err != nil {
		return BatchResult{}, err
	}
	if len(dates) == 0 {
		return BatchResult{}, fmt.Errorf("%w: в этом диапазоне схема не даёт ни одной даты", ErrInvalidInput)
	}
	return s.CreateBatch(ctx, CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creatorIDs,
		Dates:          dates,
		DraftLeadDays:  draftLeadDays,
		PerDay:         perDay,
		CreatedBy:      createdBy,
	})
}

// CreateBatch — простановка произвольного набора дат.
func (s *Service) CreateBatch(ctx context.Context, in CreateBatchInput) (BatchResult, error) {
	in.CreatorUserIDs = dedupeIDs(in.CreatorUserIDs)
	in.Dates = dedupeDates(in.Dates)
	if len(in.Dates) == 0 {
		return BatchResult{}, fmt.Errorf("%w: не выбрано ни одной даты", ErrInvalidInput)
	}
	// «Ни одного креатора» проверяет репозиторий, а не сервис: пустой
	// список — ошибка у проекта с креаторами и норма у проекта без них,
	// а вид проекта известен только там, где читается его строка.
	//
	// Потолок пачки считается по строкам, которые реально уедут в
	// INSERT: без креаторов произведение вырождается в одни даты.
	if in.PerDay <= 0 {
		in.PerDay = 1
	}
	if in.PerDay > maxPerDay {
		return BatchResult{}, fmt.Errorf("%w: роликов в день не больше %d", ErrInvalidInput, maxPerDay)
	}
	rows := len(in.Dates) * in.PerDay
	if n := len(in.CreatorUserIDs); n > 0 {
		rows *= n
	}
	if rows > maxBatch {
		return BatchResult{}, fmt.Errorf("%w: пачка на %d выкладок, потолок %d", ErrInvalidInput, rows, maxBatch)
	}
	if in.DraftLeadDays < 0 || in.DraftLeadDays > 30 {
		return BatchResult{}, fmt.Errorf("%w: срок черновика вне разумных границ", ErrInvalidInput)
	}
	return s.repo.CreateBatch(ctx, in)
}

// PreviewBatch — что будет создано, до создания. Массовое создание —
// единственное место, где одна ошибка стоит ручной чистки шестидесяти
// строк, поэтому предпросмотр обязателен (см. риск в плане).
func (s *Service) PreviewBatch(scheme Scheme, creatorIDs []uuid.UUID, from, to time.Time, perDay int) ([]time.Time, int, error) {
	dates, err := GenerateDates(scheme, from, to)
	if err != nil {
		return nil, 0, err
	}
	if perDay <= 0 {
		perDay = 1
	}
	// Дни и количество — разные числа, и предпросмотр обязан показывать
	// второе: «14 дат» при трёх роликах в день — это 42 строки в плане,
	// и узнать об этом после нажатия хуже, чем до.
	total := len(dates) * perDay
	if n := len(dedupeIDs(creatorIDs)); n > 0 {
		total *= n
	}
	return dates, total, nil
}

// AddSelfPublication — креатор заводит себе выкладку сам.
//
// Зачем это есть: план периода бывает выполнен, а до ступени просмотров
// не хватает. Дать добрать самому — дешевле, чем гонять человека к
// менеджеру за строкой в календаре. Подтверждения менеджером нет
// намеренно: согласование убивает весь смысл кнопки.
//
// Дальше такая выкладка живёт как обычная: пять площадок, ссылки,
// чеклист проекта, ежедневный сбор статистики. Отличается она ровно
// одним — не участвует в знаменателе недосдачи (см. миграцию 00053).
func (s *Service) AddSelfPublication(ctx context.Context, projectID, creatorID uuid.UUID, day, now time.Time) (Publication, error) {
	day = truncateDay(day)
	if day.Before(truncateDay(now)) {
		// Задним числом выкладки не заводят: период считается по факту
		// выхода, и дата в прошлом чинила бы уже посчитанное.
		return Publication{}, fmt.Errorf("%w: выкладку заводят на сегодня или вперёд", ErrInvalidInput)
	}
	// Год вперёд — не «выкладка», а опечатка в году.
	if day.After(truncateDay(now).AddDate(1, 0, 0)) {
		return Publication{}, fmt.Errorf("%w: дата больше чем на год вперёд", ErrInvalidInput)
	}
	return s.repo.AddSelfPublication(ctx, projectID, creatorID, day)
}

// ManagerAddPublication — менеджер ставит одну дату одному креатору.
//
// Отдельно от пачки: пачка ставит план на месяц, а это — правка плана,
// которая случается каждую неделю. Раньше её не было вовсе, и менеджер,
// которому надо добавить креатору один день, заводил пачку из одного
// креатора и одной даты — с батч-идентификатором, по которому потом
// «отменить пачку» снимало бы ровно эту строку.
func (s *Service) ManagerAddPublication(ctx context.Context, in AddPublicationInput) (Publication, error) {
	in.Day = truncateDay(in.Day)
	now := truncateDay(in.Now)
	if in.Day.Before(now) {
		// Задним числом выкладки не заводят: период считается по факту
		// выхода, и дата в прошлом чинила бы уже посчитанное.
		return Publication{}, fmt.Errorf("%w: выкладку ставят на сегодня или вперёд", ErrInvalidInput)
	}
	if in.Day.After(now.AddDate(1, 0, 0)) {
		return Publication{}, fmt.Errorf("%w: дата больше чем на год вперёд", ErrInvalidInput)
	}
	if in.DraftLeadDays < 0 || in.DraftLeadDays > 30 {
		return Publication{}, fmt.Errorf("%w: срок черновика вне разумных границ", ErrInvalidInput)
	}
	return s.repo.ManagerAddPublication(ctx, in)
}

// MoveDueDate — перенести дату одной выкладки.
//
// Двигать можно только то, по чему ещё не сдавали: у выкладки со
// ссылками ролик уже вышел, и «перенос» задним числом переписал бы
// историю периода.
//
// Открытая просьба о переносе закрывается здесь же: менеджер ответил на
// неё делом, и оставить её висеть значило бы показывать креатору, что
// его всё ещё не услышали.
func (s *Service) MoveDueDate(ctx context.Context, in MoveDueDateInput) (Publication, error) {
	in.Day = truncateDay(in.Day)
	now := truncateDay(in.Now)
	if in.Day.Before(now) {
		return Publication{}, fmt.Errorf("%w: переносят на сегодня или вперёд", ErrInvalidInput)
	}
	if in.Day.After(now.AddDate(1, 0, 0)) {
		return Publication{}, fmt.Errorf("%w: дата больше чем на год вперёд", ErrInvalidInput)
	}
	pub, err := s.repo.MoveDueDate(ctx, in)
	if err != nil {
		return pub, err
	}
	// Заказчику — если он про это просил. После транзакции и молча при
	// ошибке: перенос состоялся, и падать из-за неотправленного
	// уведомления значило бы отменить сделанное.
	s.notifyClientShift(ctx, pub, in.Now)
	return pub, nil
}

// CancelPublication — снять одну запланированную выкладку.
//
// Не то же самое, что «закрыть неполную» (CloseManually): там ролик
// вышел не везде и менеджер принимает это как факт, здесь выкладки не
// будет вовсе. Сданное не снимается: работа креатора не исчезает из-за
// правки плана.
func (s *Service) CancelPublication(ctx context.Context, pubID, actor uuid.UUID, reason string) (Publication, error) {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > 300 {
		return Publication{}, fmt.Errorf("%w: причина длиннее 300 символов", ErrInvalidInput)
	}
	pub, err := s.repo.CancelPublication(ctx, pubID, actor, reason)
	if err != nil {
		return pub, err
	}
	s.notifyClientShift(ctx, pub, time.Now())
	return pub, nil
}

// notifyClientShift — «дата изменилась» заказчику, если он подписан.
//
// Отдельным методом, потому что зовётся из двух мест: перенос и снятие
// для заказчика — один вопрос «когда теперь», и разводить их по двум
// видам уведомлений значило бы прислать ему два письма об одном.
func (s *Service) notifyClientShift(ctx context.Context, pub Publication, now time.Time) {
	if pub.ProjectID == uuid.Nil {
		return
	}
	if _, err := s.repo.NotifyClientDateShift(ctx, pub.ProjectID, pub.ID, now); err != nil {
		slog.Warn("client date shift notify failed",
			"project", pub.ProjectID, "publication", pub.ID, "err", err)
	}
}

// SubmitLinks — креатор сдаёт ролик ссылками.
func (s *Service) SubmitLinks(ctx context.Context, in SubmitLinksInput) (Publication, error) {
	if len(in.URLs) == 0 {
		return Publication{}, ErrNoLinks
	}
	// Название — человеческая подпись к ролику, а не заголовок статьи:
	// длинное всё равно обрежется в списке, и лучше сказать об этом сразу.
	in.Title = strings.TrimSpace(in.Title)
	if utf8.RuneCountInString(in.Title) > 120 {
		return Publication{}, fmt.Errorf("%w: название ролика длиннее 120 символов", ErrInvalidInput)
	}
	parsed := make([]Link, 0, len(in.URLs))
	seen := make(map[string]bool, len(in.URLs))
	for _, raw := range in.URLs {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		// Короткую ссылку из приложения разворачиваем ДО разбора: в
		// vt.tiktok.com/ZSbyuhrDf нет ни автора, ни id ролика, и
		// сборщик принимает её хвост за имя аккаунта (см. shortlinks.go).
		l, err := ParseLink(s.expandShort(ctx, raw))
		if err != nil {
			return Publication{}, fmt.Errorf("%q: %w", raw, err)
		}
		if seen[l.Platform] {
			return Publication{}, fmt.Errorf("%w: %s", ErrDuplicatePlatform, l.Platform)
		}
		seen[l.Platform] = true
		parsed = append(parsed, l)
	}
	if len(parsed) == 0 {
		return Publication{}, ErrNoLinks
	}
	in.CheckedItemIDs = dedupeIDs(in.CheckedItemIDs)
	pub, err := s.repo.SubmitLinks(ctx, in, parsed)
	if err != nil {
		return pub, err
	}
	// Ролик ВЫШЕЛ — говорим заказчику. Именно на переходе в «вышел», а
	// не на каждой досланной ссылке: заказчику важен ролик, а не то,
	// что креатор добавил четвёртую площадку.
	if pub.Status == StatusDone {
		if _, nerr := s.repo.NotifyClientNewVideo(ctx, pub.ProjectID, pub.ID, time.Now()); nerr != nil {
			slog.Warn("client new video notify failed",
				"project", pub.ProjectID, "publication", pub.ID, "err", nerr)
		}
	}
	return pub, nil
}

// ManagerEditLink — менеджер правит сданную ссылку.
//
// Зачем вообще: ссылку сдаёт креатор, и ошибается в ней тоже он —
// вставил адрес чужого ролика, мобильный домен с обрезанным id, ссылку
// на профиль вместо видео. До сих пор исправить это мог только он сам:
// менеджер видел, что цифры не собираются, и писал в чат. Теперь
// правит на месте, и правка — именная (см. событие в outbox).
//
// Пустой URL снимает ссылку с площадки: выкладка возвращается в
// «неполную», а не остаётся закрытой по ошибочной ссылке.
func (s *Service) ManagerEditLink(ctx context.Context, in ManagerEditLinkInput) (Publication, error) {
	in.ByCreator = false
	return s.editLink(ctx, in)
}

// CreatorEditLink — креатор пересылает свою ссылку.
//
// Ролик удаляют с площадки, аккаунт перевыкладывают, короткая ссылка
// протухает — и адрес есть ровно у одного человека, у автора. До сих пор
// он писал его в переписку, а менеджер переносил руками: лишний шаг, на
// котором ссылка живёт в чате, а не в сервисе.
//
// Границы у креаторского пути две, и обе намеренные:
//   - правит он ТОЛЬКО свою выкладку;
//   - СНЯТЬ ссылку не может. Пустой адрес у менеджера означает «этой
//     площадки не было», и это решение о работе, а не о ссылке: автору
//     нечего решать, вышел его ролик или нет.
func (s *Service) CreatorEditLink(ctx context.Context, in ManagerEditLinkInput) (Publication, error) {
	in.ByCreator = true
	if strings.TrimSpace(in.URL) == "" {
		return Publication{}, ErrLinkRemoveDenied
	}
	mine, err := s.repo.PublicationBelongsTo(ctx, in.PublicationID, in.ManagerUserID)
	if err != nil {
		return Publication{}, err
	}
	// Не 403: «такая выкладка есть, но не ваша» — подтверждение чужой
	// выкладки постороннему.
	if !mine {
		return Publication{}, ErrNotFound
	}
	return s.editLink(ctx, in)
}

func (s *Service) editLink(ctx context.Context, in ManagerEditLinkInput) (Publication, error) {
	in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
	if !IsKnownPlatform(in.Platform) {
		return Publication{}, fmt.Errorf("%w: неизвестная площадка %q", ErrInvalidInput, in.Platform)
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		return s.repo.ManagerRemoveLink(ctx, in)
	}
	l, err := ParseLink(s.expandShort(ctx, in.URL))
	if err != nil {
		return Publication{}, fmt.Errorf("%q: %w", in.URL, err)
	}
	// Ссылка другой площадки в этот слот не ложится: у выкладки по одной
	// ссылке на площадку, и «исправление» подменило бы не ту строку.
	if l.Platform != in.Platform {
		return Publication{}, fmt.Errorf("%w: ссылка ведёт на %s, а правим %s",
			ErrInvalidInput, l.Platform, in.Platform)
	}
	return s.repo.ManagerEditLink(ctx, in, l)
}

// CloseManually — менеджер закрывает неполную выкладку. Причина обязательна.
func (s *Service) CloseManually(ctx context.Context, in CloseManuallyInput) (Publication, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return Publication{}, fmt.Errorf("%w: закрытие неполной выкладки требует причины", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Reason) > 500 {
		return Publication{}, fmt.Errorf("%w: причина слишком длинная", ErrInvalidInput)
	}
	pub, err := s.repo.CloseManually(ctx, in)
	if err != nil {
		return pub, err
	}
	// Закрытая вручную выкладка для заказчика такой же вышедший ролик:
	// он видит её в ленте, и молчать о ней значило бы показать ролик,
	// про который не сказали.
	if _, nerr := s.repo.NotifyClientNewVideo(ctx, pub.ProjectID, pub.ID, time.Now()); nerr != nil {
		slog.Warn("client new video notify failed",
			"project", pub.ProjectID, "publication", pub.ID, "err", nerr)
	}
	return pub, nil
}

// RequestDateChange — креатор просит перенос.
func (s *Service) RequestDateChange(ctx context.Context, pubID, actorID uuid.UUID,
	newDate time.Time, reason string) (DateRequest, error) {

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DateRequest{}, fmt.Errorf("%w: нужна причина переноса", ErrInvalidInput)
	}
	if utf8.RuneCountInString(reason) > 500 {
		return DateRequest{}, fmt.Errorf("%w: причина слишком длинная", ErrInvalidInput)
	}
	if truncateDay(newDate).Before(truncateDay(time.Now())) {
		return DateRequest{}, fmt.Errorf("%w: перенос в прошлое", ErrInvalidInput)
	}
	return s.repo.RequestDateChange(ctx, pubID, actorID, newDate, reason)
}

func (s *Service) DecideDateRequest(ctx context.Context, requestID, managerID uuid.UUID, approve bool) error {
	return s.repo.DecideDateRequest(ctx, requestID, managerID, approve)
}

func (s *Service) CancelBatch(ctx context.Context, projectID, batchID uuid.UUID) (int, error) {
	return s.repo.CancelBatch(ctx, projectID, batchID)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Publication, error) {
	return s.repo.Get(ctx, id)
}

// ListForManager — все выкладки проекта.
func (s *Service) ListForManager(ctx context.Context, projectID uuid.UUID) ([]Publication, error) {
	return s.repo.ListByProject(ctx, projectID)
}

// ListForCreator — только свои выкладки. Отдельный метод, а не флаг у
// ListForManager: так труднее случайно отдать креатору чужое.
func (s *Service) ListForCreator(ctx context.Context, projectID, creatorID uuid.UUID) ([]Publication, error) {
	return s.repo.ListByCreator(ctx, projectID, creatorID)
}

func (s *Service) AddCreator(ctx context.Context, projectID, creatorID, addedBy uuid.UUID) error {
	return s.repo.AddCreator(ctx, projectID, creatorID, addedBy)
}

func (s *Service) RemoveCreator(ctx context.Context, projectID, creatorID uuid.UUID) error {
	return s.repo.RemoveCreator(ctx, projectID, creatorID)
}

func (s *Service) SnapshotChecklist(ctx context.Context, projectID, templateID, actor uuid.UUID) (int, error) {
	return s.repo.SnapshotChecklist(ctx, projectID, templateID, actor)
}

func (s *Service) ProjectChecklist(ctx context.Context, projectID uuid.UUID) ([]ChecklistItem, error) {
	return s.repo.ProjectChecklist(ctx, projectID)
}

// ---- helpers ----

func dedupeIDs(in []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(in))
	out := make([]uuid.UUID, 0, len(in))
	for _, id := range in {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func dedupeDates(in []time.Time) []time.Time {
	seen := make(map[time.Time]bool, len(in))
	out := make([]time.Time, 0, len(in))
	for _, d := range in {
		day := truncateDay(d)
		if seen[day] {
			continue
		}
		seen[day] = true
		out = append(out, day)
	}
	return out
}

func (s *Service) ManagerHasAccess(ctx context.Context, projectID, managerID uuid.UUID) error {
	return s.repo.ManagerHasAccess(ctx, projectID, managerID)
}

func (s *Service) ProjectOfPublication(ctx context.Context, pubID uuid.UUID) (uuid.UUID, error) {
	return s.repo.ProjectOfPublication(ctx, pubID)
}

func (s *Service) ProjectOfDateRequest(ctx context.Context, reqID uuid.UUID) (uuid.UUID, error) {
	return s.repo.ProjectOfDateRequest(ctx, reqID)
}

// ChecklistMeta — какой шаблон и какой версии подключён к проекту.
func (s *Service) ChecklistMeta(ctx context.Context, projectID uuid.UUID) (*ChecklistSnapshotMeta, error) {
	return s.repo.ChecklistMeta(ctx, projectID)
}
