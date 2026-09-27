package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
)

var (
	ErrNotFound = errors.New("order not found")
	// ErrNoConsent — клиент не соглашался с текущей версией правил.
	// Без согласия дальше не пускаем: иначе спор «мы такого не читали»
	// нечем закрыть.
	ErrNoConsent = errors.New("client has not accepted the current terms")
	// ErrTooManyCreators — превышено ограничение объёма: первый месяц
	// один креатор, дальше 2–3.
	ErrTooManyCreators = errors.New("creators limit exceeded for this client")
	// ErrNotEnoughCandidates — в подборке меньше, чем нужно взять.
	ErrNotEnoughCandidates = errors.New("not enough candidates in the shortlist")
	// ErrNotACreator — в подборку попал не специалист.
	ErrNotACreator = errors.New("candidate cannot be a creator")
	// ErrCreatorBusy — креатор занят в этом месяце.
	ErrCreatorBusy = errors.New("creator is not available in this month")
	// ErrWrongStatus — действие не подходит к текущему состоянию заказа.
	ErrWrongStatus = errors.New("order is in a state that does not allow this")
	// ErrNotInvited — креатор отвечает на приглашение, которого нет.
	ErrNotInvited = errors.New("no active invitation for this creator")
	// ErrNoFreeSlot — звать некого: свободных мест нет или резерв пуст.
	// Приглашение уходит только на реально свободное место, иначе
	// согласившихся окажется больше, чем мест.
	ErrNoFreeSlot = errors.New("no free slot to invite into")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// ---- правила и согласие ----

// CurrentTerms — действующая версия правил.
func (r *Repo) CurrentTerms(ctx context.Context) (Terms, error) {
	var t Terms
	err := r.db.QueryRow(ctx, `
SELECT id, version, body, published_at,
       salary_per_month, videos_first_month, videos_next_months,
       rate_per_1000_views, bonus_views_threshold, rate_per_1000_views_over,
       click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
       fee_per_video
FROM terms_versions
ORDER BY version DESC LIMIT 1`).Scan(&t.ID, &t.Version, &t.Body, &t.PublishedAt,
		&t.SalaryPerMonth, &t.VideosFirstMonth, &t.VideosNextMonths,
		&t.RatePer1000Views, &t.BonusViewsThreshold, &t.RatePer1000ViewsOver,
		&t.ClickBonusRate, &t.ClickBonusThreshold, &t.ClickBonusRateOver,
		&t.FeePerVideo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Terms{}, ErrNotFound
	}
	if err != nil {
		return Terms{}, fmt.Errorf("current terms: %w", err)
	}
	return t, nil
}

// HasConsent — клиент согласился именно с этой версией правил.
func (r *Repo) HasConsent(ctx context.Context, userID, termsID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM client_terms_consents
    WHERE user_id = $1 AND terms_version_id = $2
)`, userID, termsID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check consent: %w", err)
	}
	return ok, nil
}

// Consent — зафиксировать согласие. Повторное — не ошибка.
func (r *Repo) Consent(ctx context.Context, userID, termsID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
INSERT INTO client_terms_consents (user_id, terms_version_id)
VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, termsID)
	if err != nil {
		return fmt.Errorf("save consent: %w", err)
	}
	return nil
}

// ---- ограничение объёма ----

// CompletedPaidMonths — сколько оплаченных месяцев клиент уже ЗАВЕРШИЛ.
//
// Именно завершил: месяц считается пройденным, когда он закончился, а не
// когда за него заплатили. Иначе ограничение обходится за минуту — клиент
// платит за одного и тут же добирает троих.
// CompletedPaidMonths — сколько оплаченных месяцев у клиента будет
// закрыто к моменту asOf. Считаются только оплаченные: начатый, но не
// оплаченный заказ опытом работы не является.
func (r *Repo) CompletedPaidMonths(ctx context.Context, clientID uuid.UUID, asOf time.Time) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `
SELECT count(*) FROM creator_orders
WHERE client_user_id = $1
  AND status = 'paid'
  AND start_month + interval '1 month' <= $2`, clientID, asOf).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count paid months: %w", err)
	}
	return n, nil
}

// AllowedCreators — сколько креаторов клиент может взять сейчас.
// AllowedCreators — сколько креаторов клиент может взять НА ЭТОТ МЕСЯЦ.
//
// Месяц, а не «сегодня»: клиент выбирает, с какого месяца берёт команду,
// и к декабрю у него может быть закрыто больше месяцев, чем к сентябрю.
// Считать лимит на сегодня значило бы показывать одно число, а на
// создании заказа отказывать по другому.
func (r *Repo) AllowedCreators(ctx context.Context, clientID uuid.UUID, month time.Time) (int, error) {
	months, err := r.CompletedPaidMonths(ctx, clientID, firstOfMonth(month))
	if err != nil {
		return 0, err
	}
	if months == 0 {
		return FirstMonthCreators, nil
	}
	return MaxCreators, nil
}

// ---- занятость ----

// SetAvailability — креатор отмечает, свободен ли он в месяце.
func (r *Repo) SetAvailability(ctx context.Context, creatorID uuid.UUID, month time.Time, available bool) error {
	_, err := r.db.Exec(ctx, `
INSERT INTO creator_availability (creator_user_id, month, is_available, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (creator_user_id, month) DO UPDATE
SET is_available = EXCLUDED.is_available, updated_at = now()`,
		creatorID, firstOfMonth(month), available)
	if err != nil {
		return fmt.Errorf("set availability: %w", err)
	}
	return nil
}

// BusyCreators — кто из списка занят в этом месяце.
//
// Отсутствие отметки означает «свободен»: заставлять креатора отмечать
// каждый месяц вперёд нельзя, он просто перестанет это делать.
func (r *Repo) BusyCreators(ctx context.Context, ids []uuid.UUID, month time.Time) (map[uuid.UUID]bool, error) {
	busy := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return busy, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT creator_user_id FROM creator_availability
WHERE creator_user_id = ANY($1) AND month = $2 AND is_available = FALSE`,
		ids, firstOfMonth(month))
	if err != nil {
		return nil, fmt.Errorf("busy creators: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan busy creator: %w", err)
		}
		busy[id] = true
	}
	return busy, rows.Err()
}

// ---- заказ ----

// Create — завести заказ с подборкой в порядке приоритета.
func (r *Repo) Create(
	ctx context.Context, in CreateOrderInput, termsID, projectID uuid.UUID, now time.Time,
) (Order, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	month := firstOfMonth(in.StartMonth)
	var o Order
	err = tx.QueryRow(ctx, `
INSERT INTO creator_orders
    (client_user_id, project_kind, start_month, needed, videos_count,
     terms_version_id, project_id, status)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '00000000-0000-0000-0000-000000000000'::uuid),
        'submitted')
RETURNING id, client_user_id, project_kind, start_month, needed, videos_count, status,
          terms_version_id, project_id, paid_at, created_at, updated_at`,
		in.ClientUserID, string(in.ProjectKind), month, in.Needed, in.VideosCount,
		termsID, projectID).Scan(
		&o.ID, &o.ClientUserID, &o.ProjectKind, &o.StartMonth, &o.Needed, &o.VideosCount,
		&o.Status, &o.TermsVersionID, &o.ProjectID, &o.PaidAt, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return Order{}, fmt.Errorf("insert order: %w", err)
	}

	// Отмеченные заказчиком. Приоритет пишем позицией в списке, но он
	// больше не смысл, а порядок строк на экране: очередь приглашений
	// ушла вместе со старой логикой, и is_preferred отвечает на другой
	// вопрос — «кого хотят особенно».
	priorities := make([]int, 0, len(in.CreatorIDs))
	for i := range in.CreatorIDs {
		priorities = append(priorities, i+1)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO order_candidates (order_id, creator_user_id, priority, is_preferred)
SELECT $1, c, p, TRUE FROM unnest($2::uuid[], $3::int[]) AS t(c, p)`,
		o.ID, in.CreatorIDs, priorities); err != nil {
		return Order{}, fmt.Errorf("insert candidates: %w", err)
	}

	// Бриф — в той же транзакции: заявка без брифа и бриф без заявки
	// одинаково бесполезны, а разнести их значит однажды получить одно
	// без другого.
	if in.Brief.Filled() {
		// Площадки — пустой массив, а не NULL: пустой означает «все
		// пять», и различать его с «не выбирали» на этом поле нечем.
		// nil в pgx приезжает именно NULL, и колонка NOT NULL его
		// отвергает — на живом стенде это был 500 на кнопке «Отправить».
		platforms := in.Brief.Platforms
		if platforms == nil {
			platforms = []string{}
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO order_briefs (order_id, goal, product, audience, tone, refs, platforms)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			o.ID, in.Brief.Goal, in.Brief.Product, in.Brief.Audience,
			in.Brief.Tone, in.Brief.Refs, platforms); err != nil {
			return Order{}, fmt.Errorf("insert brief: %w", err)
		}
	}

	// Пинг менеджерам — в той же транзакции, что и сама заявка.
	//
	// Иначе появляется третье состояние: заявка есть, сообщения нет.
	// Именно оно и было до сих пор — человек нажимал «Отправить»,
	// читал «мы вам напишем» и уходил в тишину, потому что о заявке
	// никто не узнавал, пока кто-нибудь не открывал CRM.
	if err := emitSubmitted(ctx, tx, o, in, len(in.CreatorIDs)); err != nil {
		return Order{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, o.ID)
}

// emitSubmitted — сообщение в общий чат менеджеров: заявку завели.
//
// Собирает то, с чего начнётся звонок: кто заказчик и как с ним
// связаться, сколько роликов, скольких отметили, какую сумму человек
// видел на баре и что написал в брифе. Всё в payload, а не ссылкой на
// заказ: n8n не ходит к нам обратно, и сообщение, в котором один
// идентификатор, значит «откройте CRM и разберитесь сами».
func emitSubmitted(
	ctx context.Context, tx pgx.Tx, o Order, in CreateOrderInput, preferred int,
) error {
	var name, contact string
	// Имя и почта — не критичны: заявка важнее подписи под ней, и
	// молчаливый пропуск здесь честнее отката всей транзакции.
	_ = tx.QueryRow(ctx, `
SELECT COALESCE(NULLIF(cp.display_name, ''), split_part(u.email, '@', 1), ''),
       COALESCE(u.email, '')
FROM users u
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE u.id = $1`, o.ClientUserID).Scan(&name, &contact)

	payload := map[string]any{
		"order_id":     o.ID,
		"start_month":  o.StartMonth.Format("2006-01"),
		"videos_count": o.VideosCount,
		"preferred":    preferred,
		"title":        projectTitle(in.Brief),
		// Ветка воронки: у проекта без креаторов другой разговор —
		// снимаем мы, состав собирать не надо, и менеджер должен
		// понять это из сообщения, а не открыв карточку.
		"project_kind": string(o.ProjectKind),
	}
	if o.ProjectID != nil {
		payload["project_id"] = *o.ProjectID
	}
	if name != "" {
		payload["client_name"] = name
	}
	if contact != "" {
		payload["client_contact"] = contact
	}
	if in.Ceiling > 0 {
		payload["ceiling"] = in.Ceiling
	}
	if txt := in.Brief.Text(); txt != "" {
		payload["brief"] = txt
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, o.ID.String(),
		outbox.EventOrderSubmitted, payload); err != nil {
		return fmt.Errorf("emit submitted: %w", err)
	}
	return nil
}

// GetByProject — заказ, из которого вырос проект.
//
// Нужен менеджерскому экрану проекта: там видно состав, но не видно, как
// он собирался — кто отказался, кто ещё думает и кто ждёт очереди.
// Отдельной выборкой, а не фильтром списка: список менеджера отдаёт
// только застрявшие заказы (status = inviting и звать некого), а у
// проекта заказ давно оплачен и в тот список не попадает вовсе.
//
// Проект мог быть заведён руками — тогда заказа нет, и это не ошибка.
func (r *Repo) GetByProject(ctx context.Context, projectID uuid.UUID) (Order, error) {
	var id uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT id FROM creator_orders WHERE project_id = $1`, projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("order by project: %w", err)
	}
	return r.Get(ctx, id)
}

// InviteNext — позвать следующих по приоритету вручную.
//
// Обычно это делается само: отказ и сгоревшее приглашение сразу отдают
// место следующему. Ручной вызов нужен там, где автоматика не сработала
// — например, заказ остался черновиком и приглашения не ушли вовсе.
// Поэтому проверка на свободное место здесь та же, что и в автоматике:
// звать «про запас» нельзя, иначе согласившихся будет больше, чем мест.
func (r *Repo) InviteNext(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status OrderStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM creator_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("lock order: %w", err)
	}
	if status != StatusDraft && status != StatusSubmitted && status != StatusInviting {
		return Order{}, fmt.Errorf("%w: заказ уже %s", ErrWrongStatus, status)
	}

	sent, err := inviteNext(ctx, tx, orderID, now)
	if err != nil {
		return Order{}, err
	}
	// Ноль — это не «сделали ничего», а «звать некого»: либо мест нет,
	// либо резерв кончился. Тихий успех здесь читался бы как поломка
	// кнопки.
	if sent == 0 {
		return Order{}, ErrNoFreeSlot
	}
	if _, err := tx.Exec(ctx,
		// submitted — то же «ещё не звали»: заявка пришла из воронки и
		// завела проект, но приглашений не отправляла.
		`UPDATE creator_orders SET status = 'inviting', updated_at = now()
		 WHERE id = $1 AND status IN ('draft', 'submitted')`, orderID); err != nil {
		return Order{}, fmt.Errorf("mark inviting: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, orderID)
}

// Get — заказ с подборкой и посчитанными остатками.
func (r *Repo) Get(ctx context.Context, orderID uuid.UUID) (Order, error) {
	var o Order
	err := r.db.QueryRow(ctx, `
SELECT id, client_user_id, project_kind, start_month, needed, videos_count, status,
       terms_version_id, project_id, paid_at, created_at, updated_at
FROM creator_orders WHERE id = $1`, orderID).Scan(
		&o.ID, &o.ClientUserID, &o.ProjectKind, &o.StartMonth, &o.Needed, &o.VideosCount,
		&o.Status, &o.TermsVersionID, &o.ProjectID, &o.PaidAt, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("get order: %w", err)
	}

	rows, err := r.db.Query(ctx, `
SELECT c.order_id, c.creator_user_id,
       COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       ),
       c.priority, c.is_preferred, c.status, c.invited_at, c.expires_at, c.responded_at
FROM order_candidates c
LEFT JOIN users u                ON u.id = c.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = c.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = c.creator_user_id
WHERE c.order_id = $1 ORDER BY c.priority`, orderID)
	if err != nil {
		return Order{}, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.OrderID, &c.CreatorUserID, &c.CreatorName, &c.Priority, &c.IsPreferred, &c.Status,
			&c.InvitedAt, &c.ExpiresAt, &c.RespondedAt); err != nil {
			return Order{}, fmt.Errorf("scan candidate: %w", err)
		}
		o.Candidates = append(o.Candidates, c)
	}
	if err := rows.Err(); err != nil {
		return Order{}, err
	}

	for _, c := range o.Candidates {
		switch c.Status {
		case CandidateAccepted:
			o.Accepted++
		case CandidateReserve:
			o.ReserveLeft++
		}
	}
	o.NeedMore = o.Needed - o.Accepted
	if o.NeedMore < 0 {
		o.NeedMore = 0
	}
	return o, nil
}

// ---- приглашения ----

// inviteNext — заполнить свободные места приглашениями из резерва,
// строго по приоритету.
//
// Ключевое правило: приглашение уходит ТОЛЬКО на реально свободное место.
// Звать всех сразу и брать первых согласившихся быстрее, но тогда часть
// креаторов получает «места кончились», и через два-три раза они
// перестают открывать приглашения вовсе.
//
// Возвращает, скольким отправили.
func inviteNext(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, now time.Time) (int, error) {
	var needed, accepted, invited int
	err := tx.QueryRow(ctx, `
SELECT o.needed,
       count(*) FILTER (WHERE c.status = 'accepted'),
       count(*) FILTER (WHERE c.status = 'invited')
FROM creator_orders o
LEFT JOIN order_candidates c ON c.order_id = o.id
WHERE o.id = $1
GROUP BY o.needed`, orderID).Scan(&needed, &accepted, &invited)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("count slots: %w", err)
	}

	free := needed - accepted - invited
	if free <= 0 {
		return 0, nil
	}

	rows, err := tx.Query(ctx, `
UPDATE order_candidates SET status = 'invited', invited_at = $2, expires_at = $3
WHERE (order_id, creator_user_id) IN (
    SELECT order_id, creator_user_id FROM order_candidates
    WHERE order_id = $1 AND status = 'reserve'
    ORDER BY priority
    LIMIT $4
)
RETURNING creator_user_id, priority`, orderID, now, now.Add(InviteTTL), free)
	if err != nil {
		return 0, fmt.Errorf("invite candidates: %w", err)
	}
	defer rows.Close()

	type invitee struct {
		id       uuid.UUID
		priority int
	}
	sent := make([]invitee, 0, free)
	for rows.Next() {
		var iv invitee
		if err := rows.Scan(&iv.id, &iv.priority); err != nil {
			return 0, fmt.Errorf("scan invitee: %w", err)
		}
		sent = append(sent, iv)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, iv := range sent {
		if err := outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(),
			outbox.EventOrderInvitationSent, map[string]any{
				"order_id":   orderID,
				"creator_id": iv.id,
				"priority":   iv.priority,
				"expires_at": now.Add(InviteTTL),
			}); err != nil {
			return 0, fmt.Errorf("emit invitation: %w", err)
		}
	}
	return len(sent), nil
}

// SendInvitations — отправить приглашения первым по приоритету.
func (r *Repo) SendInvitations(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status OrderStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM creator_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("lock order: %w", err)
	}
	// submitted — то же «ещё не звали», просто заявка уже пришла из
	// воронки и завела проект. Пока рассылки нет (Ф3), менеджер
	// по-прежнему двигает очередь руками из карточки проекта.
	if status != StatusDraft && status != StatusSubmitted {
		return Order{}, fmt.Errorf("%w: заказ уже %s", ErrWrongStatus, status)
	}

	if _, err := inviteNext(ctx, tx, orderID, now); err != nil {
		return Order{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE creator_orders SET status = 'inviting', updated_at = now() WHERE id = $1`,
		orderID); err != nil {
		return Order{}, fmt.Errorf("mark inviting: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, orderID)
}

// Respond — креатор ответил на приглашение.
//
// При отказе место освобождается и приглашение немедленно уходит
// следующему по приоритету: клиент об этом узнаёт уведомлением, но
// ничего не делает.
func (r *Repo) Respond(ctx context.Context, orderID, creatorID uuid.UUID, accept bool, now time.Time) (Order, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Блокируем заказ, а не строку кандидата: решение меняет расклад по
	// всем местам, и два одновременных ответа не должны разъехаться.
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM creator_orders WHERE id = $1 FOR UPDATE`, orderID); err != nil {
		return Order{}, fmt.Errorf("lock order: %w", err)
	}

	newStatus := CandidateDeclined
	if accept {
		newStatus = CandidateAccepted
	}
	tag, err := tx.Exec(ctx, `
UPDATE order_candidates SET status = $3, responded_at = $4
WHERE order_id = $1 AND creator_user_id = $2 AND status = 'invited'`,
		orderID, creatorID, newStatus, now)
	if err != nil {
		return Order{}, fmt.Errorf("record response: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Order{}, ErrNotInvited
	}

	event := outbox.EventOrderInvitationDeclined
	if accept {
		event = outbox.EventOrderInvitationAccepted
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(), event,
		map[string]any{"order_id": orderID, "creator_id": creatorID}); err != nil {
		return Order{}, fmt.Errorf("emit response: %w", err)
	}

	if !accept {
		// Место освободилось — зовём следующего.
		if _, err := inviteNext(ctx, tx, orderID, now); err != nil {
			return Order{}, err
		}
	}
	if err := settleOrder(ctx, tx, orderID); err != nil {
		return Order{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, orderID)
}

// withProject — положить project_id в payload события, если он есть.
//
// Заказ живёт раньше проекта: подбор идёт до оплаты, а проект менеджер
// заводит после неё, и на момент «добрать» или «креатор молчит» проекта
// у заказа обычно ещё нет. Поэтому ключ добавляется только когда он
// заполнен: null в payload означал бы «проект есть, но неизвестно
// какой», и получатель события собрал бы ссылку в никуда.
func withProject(payload map[string]any, projectID *uuid.UUID) {
	if projectID != nil {
		payload["project_id"] = projectID.String()
	}
}

// settleOrder — пересчитать состояние заказа после изменения подборки.
//
// Состав укомплектован — staffed. Резерв кончился, а мест не хватает —
// событие «добрать»: дальше без менеджера не обойтись.
func settleOrder(ctx context.Context, tx pgx.Tx, orderID uuid.UUID) error {
	var needed, accepted, invited, reserve int
	// project_id тянем тем же запросом: он уезжает в событие «добрать»,
	// чтобы сообщение в чат вело на проект, а не заставляло искать заказ
	// руками ровно в тот момент, когда надо действовать быстро.
	var projectID *uuid.UUID
	if err := tx.QueryRow(ctx, `
SELECT o.needed, o.project_id,
       count(*) FILTER (WHERE c.status = 'accepted'),
       count(*) FILTER (WHERE c.status = 'invited'),
       count(*) FILTER (WHERE c.status = 'reserve')
FROM creator_orders o
LEFT JOIN order_candidates c ON c.order_id = o.id
WHERE o.id = $1
GROUP BY o.needed, o.project_id`, orderID).Scan(
		&needed, &projectID, &accepted, &invited, &reserve); err != nil {
		return fmt.Errorf("settle counts: %w", err)
	}

	if accepted >= needed {
		if _, err := tx.Exec(ctx, `
UPDATE creator_orders SET status = 'staffed', updated_at = now()
WHERE id = $1 AND status = 'inviting'`, orderID); err != nil {
			return fmt.Errorf("mark staffed: %w", err)
		}
		return outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(),
			outbox.EventOrderStaffed, map[string]any{"order_id": orderID})
	}

	if invited == 0 && reserve == 0 {
		// Звать больше некого. Клиенту показывается плашка «добрать»,
		// менеджеру уходит событие.
		payload := map[string]any{"order_id": orderID, "need_more": needed - accepted}
		withProject(payload, projectID)
		return outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(),
			outbox.EventOrderNeedMore, payload)
	}
	return nil
}

// ExpireAndAdvance — фоновый проход: сжечь протухшие приглашения,
// позвать следующих и сказать менеджеру про тех, кто молчит сутки.
//
// Возвращает, сколько приглашений сгорело и скольких помянули менеджеру.
func (r *Repo) ExpireAndAdvance(ctx context.Context, now time.Time) (expired, pinged int, err error) {
	// 1. Сгоревшие приглашения. Собираем заказы, которых это коснулось:
	// в каждом освободилось место, и его надо кому-то отдать.
	//
	// Только по заказам в статусе inviting — то есть по старой очереди
	// приглашений. Заявка «под ключ» живёт в submitted, приглашение в
	// ней уходит рассылкой ВСЕМ и отвечать на него никто не обязан:
	// без этой оговорки через трое суток каждая заявка сожгла бы
	// десятки «приглашений» и завалила чат пингами «креатор молчит».
	rows, err := r.db.Query(ctx, `
UPDATE order_candidates SET status = 'expired', responded_at = $1
WHERE status = 'invited' AND expires_at IS NOT NULL AND expires_at <= $1
  AND EXISTS (SELECT 1 FROM creator_orders o
               WHERE o.id = order_id AND o.status = 'inviting')
RETURNING order_id, creator_user_id`, now)
	if err != nil {
		return 0, 0, fmt.Errorf("expire invitations: %w", err)
	}
	type expiredInvite struct {
		orderID   uuid.UUID
		creatorID uuid.UUID
	}
	gone := make([]expiredInvite, 0, 8)
	for rows.Next() {
		var e expiredInvite
		if err := rows.Scan(&e.orderID, &e.creatorID); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan expired: %w", err)
		}
		gone = append(gone, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	touched := make(map[uuid.UUID]bool, len(gone))
	for _, e := range gone {
		touched[e.orderID] = true
	}

	for _, e := range gone {
		if err := r.inTx(ctx, func(tx pgx.Tx) error {
			return outbox.Emit(ctx, tx, outbox.AggregateProject, e.orderID.String(),
				outbox.EventOrderInvitationExpired, map[string]any{
					"order_id": e.orderID, "creator_id": e.creatorID,
				})
		}); err != nil {
			return len(gone), 0, err
		}
	}

	// 2. По каждому затронутому заказу зовём следующего и пересчитываем
	// состояние. Отдельными транзакциями: один заказ не должен ронять
	// обработку остальных.
	for orderID := range touched {
		if err := r.inTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`SELECT 1 FROM creator_orders WHERE id = $1 FOR UPDATE`, orderID); err != nil {
				return err
			}
			if _, err := inviteNext(ctx, tx, orderID, now); err != nil {
				return err
			}
			return settleOrder(ctx, tx, orderID)
		}); err != nil {
			return len(gone), 0, err
		}
	}

	// 3. Молчание сутки — сказать менеджеру. Один раз на приглашение:
	// manager_pinged_at не даёт написать об этом на каждом проходе.
	// project_id заказа достаём здесь же подзапросом: он уедет в событие,
	// чтобы сообщение в чат вело на проект. RETURNING не умеет джойнить,
	// поэтому скалярный подзапрос — он по первичному ключу и стоит копейки.
	pingRows, err := r.db.Query(ctx, `
UPDATE order_candidates SET manager_pinged_at = $1
WHERE status = 'invited'
  AND manager_pinged_at IS NULL
  AND invited_at IS NOT NULL
  AND invited_at <= $2
  AND EXISTS (SELECT 1 FROM creator_orders o
               WHERE o.id = order_id AND o.status = 'inviting')
RETURNING order_id, creator_user_id,
          (SELECT o.project_id FROM creator_orders o WHERE o.id = order_id)`,
		now, now.Add(-ManagerPingAfter))
	if err != nil {
		return len(gone), 0, fmt.Errorf("ping managers: %w", err)
	}
	defer pingRows.Close()
	type silent struct {
		orderID   uuid.UUID
		creatorID uuid.UUID
		projectID *uuid.UUID
	}
	quiet := make([]silent, 0, 8)
	for pingRows.Next() {
		var s silent
		if err := pingRows.Scan(&s.orderID, &s.creatorID, &s.projectID); err != nil {
			return len(gone), 0, fmt.Errorf("scan silent: %w", err)
		}
		quiet = append(quiet, s)
	}
	if err := pingRows.Err(); err != nil {
		return len(gone), 0, err
	}
	for _, s := range quiet {
		if err := r.inTx(ctx, func(tx pgx.Tx) error {
			payload := map[string]any{"order_id": s.orderID, "creator_id": s.creatorID}
			withProject(payload, s.projectID)
			return outbox.Emit(ctx, tx, outbox.AggregateProject, s.orderID.String(),
				outbox.EventOrderCandidateSilent, payload)
		}); err != nil {
			return len(gone), len(quiet), err
		}
	}

	return len(gone), len(quiet), nil
}

func (r *Repo) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AddCandidates — менеджер добирает людей в подборку, когда резерв
// кончился. Приоритеты продолжают существующие.
func (r *Repo) AddCandidates(ctx context.Context, orderID uuid.UUID, creatorIDs []uuid.UUID, now time.Time) (Order, error) {
	if len(creatorIDs) == 0 {
		return r.Get(ctx, orderID)
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status OrderStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM creator_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("lock order: %w", err)
	}
	if status != StatusDraft && status != StatusSubmitted && status != StatusInviting {
		return Order{}, fmt.Errorf("%w: заказ уже %s", ErrWrongStatus, status)
	}

	var maxPriority int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(priority), 0) FROM order_candidates WHERE order_id = $1`,
		orderID).Scan(&maxPriority); err != nil {
		return Order{}, fmt.Errorf("max priority: %w", err)
	}
	priorities := make([]int, 0, len(creatorIDs))
	for i := range creatorIDs {
		priorities = append(priorities, maxPriority+i+1)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO order_candidates (order_id, creator_user_id, priority)
SELECT $1, c, p FROM unnest($2::uuid[], $3::int[]) AS t(c, p)
ON CONFLICT (order_id, creator_user_id) DO NOTHING`,
		orderID, creatorIDs, priorities); err != nil {
		return Order{}, fmt.Errorf("add candidates: %w", err)
	}

	// Место могло быть свободно всё это время — зовём сразу.
	if status == StatusInviting {
		if _, err := inviteNext(ctx, tx, orderID, now); err != nil {
			return Order{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, orderID)
}

// CreatorCategories — категории, по которым человек считается креатором:
// снимает у себя и выкладывает на свои аккаунты, работает за оклад плюс
// бонус за просмотры.
//
// Монтажёры, дизайнеры, режиссёры и продакшн-студии сюда не входят
// намеренно — они из другой ветки («продакшн под ключ»), и в пакете
// блогеров им делать нечего. Признак берётся из существующего профиля
// (specialist_categories), отдельного поля «работает как креатор» не
// заводим: роль человек уже выбрал при регистрации.
var CreatorCategories = []string{"blogger", "ugc"}

// FilterCreators — кто из списка подходит на роль креатора.
//
// Возвращает причину отказа по каждому неподходящему, чтобы клиент увидел
// «монтажёр не берётся в пакет блогеров», а не безликое «нельзя».
func (r *Repo) FilterCreators(ctx context.Context, ids []uuid.UUID) (ok map[uuid.UUID]bool, err error) {
	ok = make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return ok, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT u.id
FROM users u
JOIN specialist_categories sc ON sc.user_id = u.id
WHERE u.id = ANY($1)
  AND u.kind IN ('specialist', 'both')
  AND u.is_active
  AND NOT u.is_manager
  AND NOT u.is_admin
  AND sc.category_code = ANY($2)
GROUP BY u.id`, ids, CreatorCategories)
	if err != nil {
		return nil, fmt.Errorf("filter creators: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan creator: %w", err)
		}
		ok[id] = true
	}
	return ok, rows.Err()
}

// ---- выдача и завершение ----

// OwnedByClient — заказ принадлежит этому заказчику.
func (r *Repo) OwnedByClient(ctx context.Context, orderID, clientID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM creator_orders WHERE id = $1 AND client_user_id = $2)`,
		orderID, clientID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check order owner: %w", err)
	}
	return ok, nil
}

// ListByClient — заказы клиента, новые сверху.
// getMany — те же заказы, что отдаёт Get, но пачкой.
//
// Списки заказов собирались циклом `Get` по каждому id: два запроса на
// заказ, то есть сорок один запрос на двадцать строк экрана. Здесь два
// запроса на весь список, а порядок сохраняется тот, в котором пришли
// идентификаторы, — его задаёт вызывающий, и ORDER BY внутри его бы
// переписал.
func (r *Repo) getMany(ctx context.Context, ids []uuid.UUID) ([]Order, error) {
	if len(ids) == 0 {
		return []Order{}, nil
	}
	byID := make(map[uuid.UUID]*Order, len(ids))

	rows, err := r.db.Query(ctx, `
SELECT id, client_user_id, project_kind, start_month, needed, videos_count, status,
       terms_version_id, project_id, paid_at, created_at, updated_at
FROM creator_orders WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("get orders: %w", err)
	}
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.ClientUserID, &o.ProjectKind, &o.StartMonth,
			&o.Needed, &o.VideosCount, &o.Status, &o.TermsVersionID, &o.ProjectID,
			&o.PaidAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan order: %w", err)
		}
		cp := o
		byID[o.ID] = &cp
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	crows, err := r.db.Query(ctx, `
SELECT c.order_id, c.creator_user_id,
       COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       ),
       c.priority, c.is_preferred, c.status, c.invited_at, c.expires_at, c.responded_at
FROM order_candidates c
LEFT JOIN users u                ON u.id = c.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = c.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = c.creator_user_id
WHERE c.order_id = ANY($1) ORDER BY c.order_id, c.priority`, ids)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var c Candidate
		if err := crows.Scan(&c.OrderID, &c.CreatorUserID, &c.CreatorName, &c.Priority,
			&c.IsPreferred, &c.Status, &c.InvitedAt, &c.ExpiresAt, &c.RespondedAt); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		if o := byID[c.OrderID]; o != nil {
			o.Candidates = append(o.Candidates, c)
		}
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}

	out := make([]Order, 0, len(ids))
	for _, id := range ids {
		o := byID[id]
		if o == nil {
			continue
		}
		for _, c := range o.Candidates {
			switch c.Status {
			case CandidateAccepted:
				o.Accepted++
			case CandidateReserve:
				o.ReserveLeft++
			}
		}
		o.NeedMore = o.Needed - o.Accepted
		if o.NeedMore < 0 {
			o.NeedMore = 0
		}
		out = append(out, *o)
	}
	return out, nil
}

func (r *Repo) ListByClient(ctx context.Context, clientID uuid.UUID, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.Query(ctx, `
SELECT id FROM creator_orders WHERE client_user_id = $1
ORDER BY created_at DESC LIMIT $2`, clientID, limit)
	if err != nil {
		return nil, fmt.Errorf("list client orders: %w", err)
	}
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan order id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return r.getMany(ctx, ids)
}

// Invitation — приглашение глазами креатора.
type Invitation struct {
	OrderID      uuid.UUID       `json:"order_id"`
	ClientUserID uuid.UUID       `json:"client_user_id"`
	StartMonth   time.Time       `json:"start_month"`
	VideosCount  int             `json:"videos_count"`
	Status       CandidateStatus `json:"status"`
	InvitedAt    *time.Time      `json:"invited_at,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	// IsPreferred — заказчик отметил этого человека: «хочу особенно
	// вас». Приписка в том же приглашении, а не отдельное письмо: два
	// сообщения одному человеку про одну заявку выглядят беспорядком.
	IsPreferred bool `json:"is_preferred"`
	// BroadcastAt — когда заявка ушла рассылкой. Отличает «позвали
	// лично» от «получил вместе со всеми», и это разные разговоры.
	BroadcastAt *time.Time `json:"broadcast_at,omitempty"`
	// Title и Brief — о чём заявка. Без них приглашение выглядит как
	// «30 роликов в октябре», и решать по нему нечего.
	Title string `json:"title,omitempty"`
	Brief string `json:"brief,omitempty"`
	// Responded — человек уже ответил, и вот как. Пусто — ещё нет.
	Responded ResponseMode `json:"responded,omitempty"`
}

// InvitationsFor — приглашения креатора: неотвеченные сверху.
//
// Сюда попадают и строки рассылки (broadcast_at), и старые именные
// приглашения. Резерв без рассылки — нет: это подборка заказчика, о
// которой человеку знать нечего, пока ему не написали.
//
// Отдаётся месяц, объём, бриф и отметка «хотят особенно» — то, чего
// достаточно для решения. Контактов заказчика нет: до согласия они не
// нужны, а после — есть проект и переписка в нём.
func (r *Repo) InvitationsFor(ctx context.Context, creatorID uuid.UUID) ([]Invitation, error) {
	rows, err := r.db.Query(ctx, `
SELECT o.id, o.client_user_id, o.start_month, o.videos_count,
       c.status, c.invited_at, c.expires_at, c.is_preferred, c.broadcast_at,
       COALESCE(NULLIF(b.product, ''), NULLIF(b.goal, ''), ''),
       COALESCE(b.goal, ''), COALESCE(b.product, ''), COALESCE(b.audience, ''),
       COALESCE(b.tone, ''), COALESCE(b.refs, ''),
       COALESCE(resp.mode, '')
FROM order_candidates c
JOIN creator_orders o        ON o.id = c.order_id
LEFT JOIN order_briefs b     ON b.order_id = o.id
LEFT JOIN order_candidate_responses resp
       ON resp.order_id = c.order_id AND resp.creator_user_id = c.creator_user_id
WHERE c.creator_user_id = $1
  AND (c.status <> 'reserve' OR c.broadcast_at IS NOT NULL)
  AND o.status <> 'cancelled'
ORDER BY (resp.mode IS NULL) DESC,
         COALESCE(c.broadcast_at, c.invited_at, c.created_at) DESC`, creatorID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()
	out := make([]Invitation, 0, 8)
	for rows.Next() {
		var (
			iv                                  Invitation
			goal, product, audience, tone, refs string
			mode                                string
		)
		if err := rows.Scan(&iv.OrderID, &iv.ClientUserID, &iv.StartMonth,
			&iv.VideosCount, &iv.Status, &iv.InvitedAt, &iv.ExpiresAt,
			&iv.IsPreferred, &iv.BroadcastAt, &iv.Title,
			&goal, &product, &audience, &tone, &refs, &mode); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		iv.Brief = OrderBrief{
			Goal: goal, Product: product, Audience: audience, Tone: tone, Refs: refs,
		}.Text()
		iv.Responded = ResponseMode(mode)
		out = append(out, iv)
	}
	return out, rows.Err()
}

// NeedingAttention — заказы, где без менеджера не обойтись: резерв
// кончился, а состав не собран.
func (r *Repo) NeedingAttention(ctx context.Context) ([]Order, error) {
	rows, err := r.db.Query(ctx, `
SELECT o.id
FROM creator_orders o
JOIN order_candidates c ON c.order_id = o.id
WHERE o.status = 'inviting'
GROUP BY o.id, o.needed
HAVING count(*) FILTER (WHERE c.status = 'accepted') < o.needed
   AND count(*) FILTER (WHERE c.status IN ('invited', 'reserve')) = 0
ORDER BY o.created_at
-- Потолок: это список «разобрать руками», и если он длиннее двух
-- сотен, проблема не в том, что не показали двести первый.
LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("orders needing attention: %w", err)
	}
	ids := make([]uuid.UUID, 0, 8)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan order id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return r.getMany(ctx, ids)
}

// Cancel — клиент распускает состав. Приглашения отзываются.
func (r *Repo) Cancel(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status OrderStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM creator_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("lock order: %w", err)
	}
	if status == StatusPaid || status == StatusCancelled {
		return Order{}, fmt.Errorf("%w: заказ уже %s", ErrWrongStatus, status)
	}

	// Приглашённые получают уведомление: их позвали и передумали, и
	// узнать об этом они должны от нас, а не по молчанию.
	rows, err := tx.Query(ctx, `
UPDATE order_candidates SET status = 'expired', responded_at = $2
WHERE order_id = $1 AND status = 'invited'
RETURNING creator_user_id`, orderID, now)
	if err != nil {
		return Order{}, fmt.Errorf("revoke invitations: %w", err)
	}
	revoked := make([]uuid.UUID, 0, 4)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Order{}, fmt.Errorf("scan revoked: %w", err)
		}
		revoked = append(revoked, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Order{}, err
	}
	for _, id := range revoked {
		if err := outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(),
			outbox.EventOrderInvitationExpired, map[string]any{
				"order_id": orderID, "creator_id": id, "reason": "order_cancelled",
			}); err != nil {
			return Order{}, fmt.Errorf("emit revoke: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE creator_orders SET status = 'cancelled', cancelled_at = $2, updated_at = now()
WHERE id = $1`, orderID, now); err != nil {
		return Order{}, fmt.Errorf("cancel order: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, orderID)
}

// MarkPaid — менеджер отмечает оплату.
//
// Платежей в системе нет (см. CLAUDE.md), поэтому отметка ручная. Проект
// после этого менеджер создаёт сам существующими ручками: авто-создание
// не делаем, пока оплата не настоящая.
func (r *Repo) MarkPaid(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	tag, err := r.db.Exec(ctx, `
UPDATE creator_orders SET status = 'paid', paid_at = $2, updated_at = now()
WHERE id = $1 AND status = 'staffed'`, orderID, now)
	if err != nil {
		return Order{}, fmt.Errorf("mark paid: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Либо заказа нет, либо состав ещё не собран: оплачивать
		// неукомплектованный состав нельзя.
		return Order{}, fmt.Errorf("%w: оплатить можно только укомплектованный заказ", ErrWrongStatus)
	}
	return r.Get(ctx, orderID)
}

// LinkProject — привязать созданный проект к заказу.
func (r *Repo) LinkProject(ctx context.Context, orderID, projectID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE creator_orders SET project_id = $2, updated_at = now() WHERE id = $1`,
		orderID, projectID)
	if err != nil {
		return fmt.Errorf("link project: %w", err)
	}
	return nil
}

// MyAvailability — своя занятость по месяцам, начиная с текущего.
//
// Запись была, чтения не было: креатор отмечал месяц и не видел, что
// отметил, — интерфейс показывал прочерк до следующего клика. Месяцы, по
// которым он ничего не говорил, в выдачу не попадают: «не отмечал» и
// «свободен» — разные вещи, и склеивать их в false нельзя.
func (r *Repo) MyAvailability(ctx context.Context, creatorID uuid.UUID, months int) ([]Availability, error) {
	if months <= 0 || months > 24 {
		months = 12
	}
	rows, err := r.db.Query(ctx, `
SELECT creator_user_id, month, is_available
FROM creator_availability
WHERE creator_user_id = $1
  AND month >= date_trunc('month', CURRENT_DATE)
  AND month < date_trunc('month', CURRENT_DATE) + make_interval(months => $2)
ORDER BY month`, creatorID, months)
	if err != nil {
		return nil, fmt.Errorf("list availability: %w", err)
	}
	defer rows.Close()
	out := make([]Availability, 0)
	for rows.Next() {
		var a Availability
		if err := rows.Scan(&a.CreatorUserID, &a.Month, &a.IsAvailable); err != nil {
			return nil, fmt.Errorf("scan availability: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- бриф ----

// LoadBrief — бриф заказа. Пустой бриф — нормальное состояние: человек
// мог отправить заявку, не заполнив ни строки, и дописать её потом.
func (r *Repo) LoadBrief(ctx context.Context, orderID uuid.UUID) (OrderBrief, error) {
	var b OrderBrief
	err := r.db.QueryRow(ctx, `
SELECT goal, product, audience, tone, refs, platforms
FROM order_briefs WHERE order_id = $1`, orderID).
		Scan(&b.Goal, &b.Product, &b.Audience, &b.Tone, &b.Refs, &b.Platforms)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderBrief{}, nil
	}
	if err != nil {
		return OrderBrief{}, fmt.Errorf("load brief: %w", err)
	}
	return b, nil
}

// SaveBrief — переписать бриф целиком.
//
// Целиком, а не по полям: бриф правят как текст — стирают лишнее и
// дописывают новое, — и «обнови присланное» оставило бы стёртую строку
// жить в задании креатора.
func (r *Repo) SaveBrief(ctx context.Context, orderID uuid.UUID, b OrderBrief) error {
	if b.Platforms == nil {
		b.Platforms = []string{}
	}
	_, err := r.db.Exec(ctx, `
INSERT INTO order_briefs (order_id, goal, product, audience, tone, refs, platforms)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (order_id) DO UPDATE
SET goal = EXCLUDED.goal, product = EXCLUDED.product, audience = EXCLUDED.audience,
    tone = EXCLUDED.tone, refs = EXCLUDED.refs, platforms = EXCLUDED.platforms,
    updated_at = now()`,
		orderID, b.Goal, b.Product, b.Audience, b.Tone, b.Refs, b.Platforms)
	if err != nil {
		return fmt.Errorf("save brief: %w", err)
	}
	return nil
}

// RemoveCandidate — убрать человека из заявки.
//
// Только пока он не согласился. Согласившийся уже в составе проекта, и
// «убрать» для него значит другое — вывести из состава (см.
// publications.RemoveCreator). Стереть здесь строку означало бы забыть,
// что человек брал заявку, при том что его выкладки остались в проекте.
func (r *Repo) RemoveCandidate(ctx context.Context, orderID, creatorID uuid.UUID) error {
	var status CandidateStatus
	err := r.db.QueryRow(ctx, `
DELETE FROM order_candidates
WHERE order_id = $1 AND creator_user_id = $2 AND status <> 'accepted'
RETURNING status`, orderID, creatorID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Либо такого в заявке нет, либо он согласился. Второе — не
		// ошибка данных, а другое действие, и сказать надо именно это.
		var accepted bool
		if err := r.db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM order_candidates
               WHERE order_id = $1 AND creator_user_id = $2 AND status = 'accepted')`,
			orderID, creatorID).Scan(&accepted); err != nil {
			return fmt.Errorf("check candidate: %w", err)
		}
		if accepted {
			return fmt.Errorf("%w: человек уже согласился — выводите из состава проекта",
				ErrWrongStatus)
		}
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("remove candidate: %w", err)
	}
	return nil
}
