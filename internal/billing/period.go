package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
	"marketpclce/internal/outbox"
	"marketpclce/internal/ratings"
)

// Период проекта: месяц работы креаторов, отсчитанный от первой
// публикации, а не от первого числа календаря.
//
// Проект с креаторами идёт один, а внутри катятся периоды. Первый
// начинается датой первой публикации и длится ровно месяц: вышел первый
// ролик 15-го — периоды идут с 15-го по 14-е. Календарный месяц делил
// пополам и съёмку, и оплату: половина роликов в одном месяце, половина
// в другом, а работа одна.
//
// Период фиксируется через две недели после своего конца: суммы
// пересчитываются в последний раз, просмотры сохраняются срезом — по
// каждой выкладке и каждой площадке, на дату отсечки. После этого числа
// периода не меняются, даже если ролики продолжают набирать просмотры.

// Состояния периода. Текстом, а не enum'ом: тариф ещё не утверждён, и
// состояний может прибавиться.
const (
	// PeriodOpen — период идёт: всё считается на лету и помечается
	// «предварительно».
	PeriodOpen = "open"
	// PeriodLocked — подытожен: суммы и просмотры больше не меняются.
	PeriodLocked = "locked"
)

// DefaultPeriodLockDelay — через сколько после конца периода он
// подытоживается сам.
//
// Четырнадцать дней — правило площадки: за две недели ролик набирает
// основную массу просмотров, дальше счёт почти не меняется.
const DefaultPeriodLockDelay = 14 * 24 * time.Hour

// ErrNoPeriods — у проекта ещё не вышло ни одного ролика, и периодов
// нет. Не ошибка данных: проект завели, выкладки запланировали, но
// отсчёт начнётся с первой публикации.
var ErrNoPeriods = errors.New("у проекта ещё нет периодов: не вышло ни одного ролика")

// ProjectPeriod — период проекта.
type ProjectPeriod struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	// Seq — какой это период по счёту: первый, второй, третий.
	Seq int `json:"seq"`
	// StartsOn/EndsOn — границы, обе включительно.
	StartsOn time.Time `json:"starts_on"`
	EndsOn   time.Time `json:"ends_on"`
	// PrevPeriodID — предыдущий период. nil только у первого: остаток
	// ступени переносится по цепочке, и она должна быть явной.
	PrevPeriodID *uuid.UUID `json:"prev_period_id,omitempty"`

	Status   string     `json:"status"`
	LockedAt *time.Time `json:"locked_at,omitempty"`
	// LockedBy — кто подытожил. nil = фоновая задача; ручного подытога
	// нет вовсе, так что сейчас здесь всегда nil.
	LockedBy *uuid.UUID `json:"locked_by,omitempty"`
	// SnapshotAsOf — на какую дату сняты числа: конец периода плюс две
	// недели. Это отсечка, а не момент фиксации: воркер мог опоздать, и
	// числа всё равно должны быть теми, что были на отсечку.
	SnapshotAsOf *time.Time `json:"snapshot_as_of,omitempty"`
	// SnapshotApprox — числам не на что опереться: поденный ряд к моменту
	// фиксации уже схлопнули.
	//
	// Не путать с «предварительно» (Accrual.IsPreview): то про период,
	// который ещё идёт, это — про период, который подытожен, но
	// опирается на пустоту. Могут стоять одновременно, и в интерфейсе их
	// нельзя схлопывать в одну плашку.
	SnapshotApprox bool `json:"snapshot_approx,omitempty"`

	// Carry* — перенос остатка ступени тарифа, в просмотрах.
	//
	// Ступень — 100 000 просмотров; остаток, не добравший до полной,
	// едет в следующий период и складывается с его просмотрами. Стороны
	// разные: клиентская и креаторская считаются по своим ставкам и
	// разъедутся, одно число на двоих скрыло бы это.
	//
	// Сейчас нули: арифметику включат, когда утвердят тариф. Место под
	// неё — в подытоженном периоде, потому что перенос такая же
	// замороженная величина, как просмотры.
	CarryInClient   int64 `json:"carry_in_client"`
	CarryOutClient  int64 `json:"carry_out_client"`
	CarryInCreator  int64 `json:"carry_in_creator"`
	CarryOutCreator int64 `json:"carry_out_creator"`

	// ClientDebtIn/ClientDebtOut — долг перед клиентом в ПРОСМОТРАХ:
	// недобрали гарантию — период оплачен как гарантия, а недостающее
	// добираем бесплатно в следующем. Не в деньгах, так в оферте.
	ClientDebtIn  int64 `json:"client_debt_in"`
	ClientDebtOut int64 `json:"client_debt_out"`

	// RatingScaleID — какой версией справочника порогов оценивался
	// период. Проставляется при подытоге и больше не меняется: чем
	// оценивали, тем и оценивали. nil у периода, который ещё идёт.
	RatingScaleID *uuid.UUID `json:"rating_scale_id,omitempty"`
	// RatingScaleVersion — её номер: человеку показывают «оценено по
	// версии 3», а не uuid.
	RatingScaleVersion *int `json:"rating_scale_version,omitempty"`
}

// IsLocked — период подытожен.
func (p ProjectPeriod) IsLocked() bool { return p.Status == PeriodLocked }

// Contains — принадлежит ли дата периоду. Границы включительные с обеих
// сторон: ролик, вышедший в последний день, принадлежит этому периоду, а
// вышедший на следующий — уже следующему.
func (p ProjectPeriod) Contains(day time.Time) bool {
	d := dayOf(day)
	return !d.Before(p.StartsOn) && !d.After(p.EndsOn)
}

// LockDueAt — момент, когда период подытоживается: конец плюс отсрочка.
func (p ProjectPeriod) LockDueAt(delay time.Duration) time.Time {
	return p.EndsOn.AddDate(0, 0, 1).Add(delay)
}

const periodScanCols = `id, project_id, seq, starts_on, ends_on, prev_period_id,
       status, locked_at, locked_by, snapshot_as_of, snapshot_approx,
       carry_in_client, carry_out_client, carry_in_creator, carry_out_creator,
       client_debt_in, client_debt_out,
       rating_scale_id,
       (SELECT rs.version FROM rating_scales rs WHERE rs.id = rating_scale_id)`

func scanPeriod(row pgx.Row) (ProjectPeriod, error) {
	var p ProjectPeriod
	err := row.Scan(&p.ID, &p.ProjectID, &p.Seq, &p.StartsOn, &p.EndsOn, &p.PrevPeriodID,
		&p.Status, &p.LockedAt, &p.LockedBy, &p.SnapshotAsOf, &p.SnapshotApprox,
		&p.CarryInClient, &p.CarryOutClient, &p.CarryInCreator, &p.CarryOutCreator,
		&p.ClientDebtIn, &p.ClientDebtOut,
		&p.RatingScaleID, &p.RatingScaleVersion)
	return p, err
}

// dayOf — дата без времени, в UTC. Границы периода — календарные дни, и
// сравнивать их с моментом времени напрямую значит ошибиться на часовом
// поясе ровно один раз в сутки.
func dayOf(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// FirstPublishedOn — день, с которого начинается отсчёт периодов проекта:
// дата первой публикации.
//
// Фактическая дата публикации, а где источник её не отдал — момент сдачи
// ссылки. Плановая due_date не годится: это план, а не факт, и считать
// по нему значит начать отсчёт от ролика, который мог не выйти.
//
// Возвращает ErrNoPeriods, если не вышло ещё ничего.
func (r *Repo) FirstPublishedOn(ctx context.Context, projectID uuid.UUID) (time.Time, error) {
	var day *time.Time
	if err := r.db.QueryRow(ctx, `
SELECT MIN(COALESCE(l.published_at, l.submitted_at))::date
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
WHERE p.project_id = $1 AND p.status <> 'cancelled'`, projectID).Scan(&day); err != nil {
		return time.Time{}, fmt.Errorf("first publication date: %w", err)
	}
	if day == nil {
		return time.Time{}, ErrNoPeriods
	}
	return dayOf(*day), nil
}

// EnsurePeriods — материализовать периоды проекта по дату upTo
// включительно.
//
// Периоды заводятся строками, а не считаются на лету, по двум причинам.
// Первая: порядковый номер и ссылка на предыдущий — часть данных, на них
// будут стоять пороги тарифа и перенос остатка. Вторая: якорь отсчёта
// замораживается первой материализацией. Дата публикации может приехать
// позже сдачи ссылки и оказаться более ранней — но если периоды уже
// заведены, границы не поедут: сдвинуть их значило бы переписать
// подытоженное.
func (r *Repo) EnsurePeriods(ctx context.Context, projectID uuid.UUID, upTo time.Time) error {
	first, err := r.FirstPublishedOn(ctx, projectID)
	if errors.Is(err, ErrNoPeriods) {
		return nil // нечего материализовать, и это нормально
	}
	if err != nil {
		return err
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Якорь: если период уже есть, начало отсчёта берём из него.
	var (
		anchor   time.Time
		lastSeq  int
		lastID   *uuid.UUID
		haveLast bool
	)
	var (
		aStart time.Time
		aSeq   int
		aID    uuid.UUID
	)
	err = tx.QueryRow(ctx, `
SELECT starts_on, seq, id FROM project_periods
WHERE project_id = $1 ORDER BY seq DESC LIMIT 1`, projectID).Scan(&aStart, &aSeq, &aID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		anchor = first
		lastSeq = 0
	case err != nil:
		return fmt.Errorf("load last period: %w", err)
	default:
		// Якорь — начало первого периода; последний известен по seq.
		anchor = aStart.AddDate(0, -(aSeq - 1), 0)
		lastSeq = aSeq
		lastID = &aID
		haveLast = true
	}

	limit := dayOf(upTo)
	for seq := lastSeq + 1; ; seq++ {
		starts := anchor.AddDate(0, seq-1, 0)
		if starts.After(limit) {
			break
		}
		ends := anchor.AddDate(0, seq, 0).AddDate(0, 0, -1)
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `
INSERT INTO project_periods (project_id, seq, starts_on, ends_on, prev_period_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`, projectID, seq, starts, ends, lastID).Scan(&id); err != nil {
			return fmt.Errorf("insert period %d: %w", seq, err)
		}
		lastID = &id
		haveLast = true
		// Защита от бесконечного цикла на испорченных данных: периодов
		// больше тысячи быть не может — это восемьдесят лет работы.
		if seq > 1000 {
			return fmt.Errorf("too many periods for project %s", projectID)
		}
	}
	_ = haveLast
	return tx.Commit(ctx)
}

// Periods — все периоды проекта, от первого к последнему.
func (r *Repo) Periods(ctx context.Context, projectID uuid.UUID) ([]ProjectPeriod, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+periodScanCols+` FROM project_periods WHERE project_id = $1 ORDER BY seq`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list periods: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectPeriod, 0, 8)
	for rows.Next() {
		p, err := scanPeriod(rows)
		if err != nil {
			return nil, fmt.Errorf("scan period: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PeriodBySeq — период по порядковому номеру.
func (r *Repo) PeriodBySeq(ctx context.Context, projectID uuid.UUID, seq int) (ProjectPeriod, error) {
	p, err := scanPeriod(r.db.QueryRow(ctx,
		`SELECT `+periodScanCols+` FROM project_periods WHERE project_id = $1 AND seq = $2`,
		projectID, seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectPeriod{}, ErrNoPeriods
	}
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("period by seq: %w", err)
	}
	return p, nil
}

// PeriodOn — период, которому принадлежит день. Последний период, если
// день уже за его границей: проект мог не выкладывать ничего месяцами,
// а спрашивают всё равно про «текущий».
func (r *Repo) PeriodOn(ctx context.Context, projectID uuid.UUID, day time.Time) (ProjectPeriod, error) {
	d := dayOf(day)
	p, err := scanPeriod(r.db.QueryRow(ctx, `
SELECT `+periodScanCols+` FROM project_periods
WHERE project_id = $1 AND starts_on <= $2
ORDER BY seq DESC LIMIT 1`, projectID, d))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectPeriod{}, ErrNoPeriods
	}
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("period on day: %w", err)
	}
	return p, nil
}

// LockPeriod — подытожить период: срез просмотров на отсечку плюс
// отметка.
//
// Идемпотентна: повторный вызов на подытоженном периоде ничего не меняет
// и ошибкой не считается.
func (r *Repo) LockPeriod(ctx context.Context, periodID uuid.UUID, actor *uuid.UUID, asOf, now time.Time) (ProjectPeriod, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := scanPeriod(tx.QueryRow(ctx,
		`SELECT `+periodScanCols+` FROM project_periods WHERE id = $1 FOR UPDATE`, periodID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectPeriod{}, ErrNoPeriods
	}
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("lock period row: %w", err)
	}
	if p.IsLocked() {
		if err := tx.Commit(ctx); err != nil {
			return ProjectPeriod{}, fmt.Errorf("commit: %w", err)
		}
		return p, nil
	}

	approx, err := snapshotPeriod(ctx, tx, p, asOf)
	if err != nil {
		return ProjectPeriod{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE project_periods
SET status = $2, locked_at = $3, locked_by = $4,
    snapshot_as_of = $5::date, snapshot_approx = $6, updated_at = now()
WHERE id = $1`, periodID, PeriodLocked, now, actor, asOf, approx); err != nil {
		return ProjectPeriod{}, fmt.Errorf("mark period locked: %w", err)
	}
	// Пороги оценок замораживаются вместе с просмотрами и суммами:
	// оценка — утверждение о прошлом, и выпуск новой шкалы не должен
	// переписывать то, что клиент уже видел.
	if err := ratings.StampPeriod(ctx, tx, periodID, p.ProjectID); err != nil {
		return ProjectPeriod{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectPeriod{}, fmt.Errorf("commit: %w", err)
	}
	return r.PeriodBySeq(ctx, p.ProjectID, p.Seq)
}

// snapshotPeriod — срез периода: какие ролики в нём вышли и сколько
// просмотров было у каждой площадки НА ОТСЕЧКУ.
//
// Принадлежность периоду — по фактической дате публикации, а где её нет —
// по сдаче ссылки. Плановая due_date не используется: платить по плану
// значит платить за ролик, который мог не выйти.
//
// Просмотры накопительные, поэтому берём последнюю строку ряда, не позже
// отсечки: не сумму по дням (завысила бы в разы) и не последнюю вообще
// (она может быть уже после отсечки). Строки на отсечку нет — пишем
// NULL, а не сегодняшнее число: пустое честнее завышенного.
//
// Возвращает признак приблизительности: поденный ряд схлопнут, и
// восстанавливать нечего.
func snapshotPeriod(ctx context.Context, tx pgx.Tx, p ProjectPeriod, asOf time.Time) (bool, error) {
	// Пересобираем срез с нуля: после переоткрытия и повторного подытога
	// старые строки не должны смешаться с новыми.
	if _, err := tx.Exec(ctx, `DELETE FROM project_period_views WHERE period_id = $1`, p.ID); err != nil {
		return false, fmt.Errorf("clear views snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_period_publications WHERE period_id = $1`, p.ID); err != nil {
		return false, fmt.Errorf("clear publications snapshot: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_period_publications
    (period_id, publication_id, creator_user_id, status, self_added, published_on)
SELECT $1, f.id, f.creator_user_id, f.status, f.self_added, f.published_on
FROM (`+publishedInPeriodSQL+`) f`,
		p.ID, p.ProjectID, p.StartsOn, p.EndsOn); err != nil {
		return false, fmt.Errorf("snapshot publications: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_period_views
    (period_id, publication_id, platform, link_id, views, likes, comments, shares, stat_date, published_at)
SELECT $1, f.id, l.platform, l.id,
       v.views, v.likes, v.comments, v.shares, v.stat_date, l.published_at
FROM (`+publishedInPeriodSQL+`) f
JOIN publication_links l ON l.publication_id = f.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments, shares, stat_date
    FROM video_stat_daily d
    WHERE d.link_id = l.id AND d.stat_date <= $5::date
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE`, p.ID, p.ProjectID, p.StartsOn, p.EndsOn, asOf); err != nil {
		return false, fmt.Errorf("snapshot views: %w", err)
	}

	// Приблизительный срез — это когда числа не просто отсутствуют, а
	// УНИЧТОЖЕНЫ: поденный ряд проекта схлопнут (есть итоговый снимок), и
	// на отсечку не нашлось ничего. Просто «ещё не собрали» —
	// не приблизительность, а честное отсутствие данных.
	var approx bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM project_stat_summary s WHERE s.project_id = $1)
   AND EXISTS (
       SELECT 1 FROM project_period_views v
       WHERE v.period_id = $2 AND v.stat_date IS NULL
   )`, p.ProjectID, p.ID).Scan(&approx); err != nil {
		return false, fmt.Errorf("detect approximate snapshot: %w", err)
	}
	return approx, nil
}

// publishedInPeriodSQL — выкладки, вышедшие в границах периода.
//
// Одно определение на весь пакет: срез и расчёт обязаны понимать «ролик
// этого периода» одинаково, иначе подытог разойдётся с тем, за что
// платят. Параметры: $1 — id проекта, $2/$3 — начало и конец периода
// (конец включительно).
//
// Дата публикации — фактическая; где источник её не отдал, берём момент
// сдачи ссылки. У выкладки без ссылок даты нет вовсе, и ни в какой
// период она не попадает: ролик не вышел.
const publishedInPeriodSQL = `
    SELECT p.id, p.creator_user_id, p.status::text AS status, p.self_added,
           MIN(COALESCE(l.published_at, l.submitted_at))::date AS published_on
    FROM project_publications p
    JOIN publication_links l ON l.publication_id = p.id
    WHERE p.project_id = $2 AND p.status <> 'cancelled'
    GROUP BY p.id, p.creator_user_id, p.status, p.self_added
    HAVING MIN(COALESCE(l.published_at, l.submitted_at))::date BETWEEN $3 AND $4
`

// SyncCarryIn — подтянуть периоду то, что оставил предыдущий: долг
// перед клиентом и перенесённый остаток креатора.
//
// Только пока период идёт: у подытоженного вход заморожен вместе со
// срезом. Предыдущий период при этом может быть ещё открыт — тогда вход
// пересчитается на следующем пересчёте, и это правильно: пока оба идут,
// числа обоих ещё не окончательные.
func (r *Repo) SyncCarryIn(ctx context.Context, p ProjectPeriod) (ProjectPeriod, error) {
	if p.IsLocked() || p.PrevPeriodID == nil {
		return p, nil
	}
	if _, err := r.db.Exec(ctx, `
UPDATE project_periods cur
SET client_debt_in = prev.client_debt_out,
    carry_in_creator = prev.carry_out_creator,
    updated_at = now()
FROM project_periods prev
WHERE cur.id = $1 AND prev.id = $2 AND cur.status = $3`,
		p.ID, *p.PrevPeriodID, PeriodOpen); err != nil {
		return ProjectPeriod{}, fmt.Errorf("sync carry in: %w", err)
	}
	return r.PeriodBySeq(ctx, p.ProjectID, p.Seq)
}

// SaveLeftovers — записать периоду то, что он оставляет следующему:
// долг перед клиентом и перенесённый остаток креатора.
//
// Только пока период идёт: у подытоженного эти величины заморожены
// вместе со срезом, и переписать их значило бы поехать цепочкой по уже
// оплаченным периодам.
func (r *Repo) SaveLeftovers(ctx context.Context, periodID uuid.UUID, l periodLeftovers) error {
	if _, err := r.db.Exec(ctx, `
UPDATE project_periods
SET client_debt_out = $2, carry_out_creator = $3, updated_at = now()
WHERE id = $1 AND status = $4`, periodID, l.ClientDebtOut, l.CreatorCarryOut, PeriodOpen); err != nil {
		return fmt.Errorf("save period leftovers: %w", err)
	}
	return nil
}

// UnlockPeriod — вернуть подытоженный период в работу.
//
// Только админ и только со следом в журнале: период, который нельзя
// переоткрыть, — это тупик, а переоткрытый молча — разъехавшиеся числа
// без объяснения. Срез при этом удаляется: он перестал быть правдой.
//
// ПОСЛЕДСТВИЕ, о котором нужно помнить: у следующего периода вход
// (carry_in_*) посчитан от выхода этого. Переоткрыли — значит вход
// следующего стал недостоверным, и цепочка дальше тоже. Пока арифметики
// переноса нет, это только предупреждение; когда появится, придётся
// решать — пересчитывать цепочку или запрещать переоткрытие периода, за
// которым уже есть подытоженные.
func (r *Repo) UnlockPeriod(ctx context.Context, periodID uuid.UUID, actor uuid.UUID, reason string) (ProjectPeriod, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := scanPeriod(tx.QueryRow(ctx,
		`SELECT `+periodScanCols+` FROM project_periods WHERE id = $1 FOR UPDATE`, periodID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectPeriod{}, ErrNoPeriods
	}
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("lock period row: %w", err)
	}
	if !p.IsLocked() {
		// Период и так идёт — переоткрывать нечего. Не ошибка: кнопку
		// могли нажать дважды.
		if err := tx.Commit(ctx); err != nil {
			return ProjectPeriod{}, fmt.Errorf("commit: %w", err)
		}
		return p, nil
	}

	var views int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM project_period_views WHERE period_id = $1`, periodID).Scan(&views); err != nil {
		return ProjectPeriod{}, fmt.Errorf("count snapshot: %w", err)
	}
	// Есть ли дальше подытоженные периоды — важно знать тому, кто
	// переоткрывает: их вход считался от выхода этого.
	var lockedAfter int
	if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM project_periods
WHERE project_id = $1 AND seq > $2 AND status = $3`,
		p.ProjectID, p.Seq, PeriodLocked).Scan(&lockedAfter); err != nil {
		return ProjectPeriod{}, fmt.Errorf("count locked after: %w", err)
	}

	if _, err := tx.Exec(ctx, `
UPDATE project_periods
SET status = $2, locked_at = NULL, locked_by = NULL,
    snapshot_as_of = NULL, snapshot_approx = FALSE, updated_at = now()
WHERE id = $1`, periodID, PeriodOpen); err != nil {
		return ProjectPeriod{}, fmt.Errorf("unlock period: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_period_views WHERE period_id = $1`, periodID); err != nil {
		return ProjectPeriod{}, fmt.Errorf("drop views snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_period_publications WHERE period_id = $1`, periodID); err != nil {
		return ProjectPeriod{}, fmt.Errorf("drop publications snapshot: %w", err)
	}
	if err := audit.Write(ctx, tx, actor, audit.ActionPeriodUnlock,
		audit.ObjectProject, p.ProjectID.String(), map[string]any{
			"period_seq":           p.Seq,
			"starts_on":            p.StartsOn.Format("2006-01-02"),
			"ends_on":              p.EndsOn.Format("2006-01-02"),
			"snapshot_rows":        views,
			"locked_periods_after": lockedAfter,
			"reason":               reason,
		}); err != nil {
		return ProjectPeriod{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectPeriod{}, fmt.Errorf("commit: %w", err)
	}
	return r.PeriodBySeq(ctx, p.ProjectID, p.Seq)
}

// ProjectsWithPublications — проекты, у которых хоть что-то вышло.
//
// Нужны фоновой задаче, чтобы материализовать периоды: пока ролик не
// вышел, отсчитывать не от чего. LIMIT — страховка от прохода по всей
// базе одним тиком; проектов такого вида десятки, и до потолка далеко.
func (r *Repo) ProjectsWithPublications(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.Query(ctx, `
SELECT DISTINCT p.project_id
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
WHERE p.status <> 'cancelled'
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("projects with publications: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DueForLock — периоды, которым пора подытожиться: период кончился
// больше delay назад, а подытога нет.
func (r *Repo) DueForLock(ctx context.Context, now time.Time, delay time.Duration, limit int) ([]ProjectPeriod, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, `
SELECT `+periodScanCols+`
FROM project_periods
WHERE status = $1
  AND (ends_on + interval '1 day' + $2::interval) <= $3
ORDER BY ends_on
LIMIT $4`, PeriodOpen, delay, now, limit)
	if err != nil {
		return nil, fmt.Errorf("periods due for lock: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectPeriod, 0, limit)
	for rows.Next() {
		p, err := scanPeriod(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due period: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EmitPeriodClosed — событие «период подытожен» в outbox.
//
// Отдельной транзакцией, уже после подытога и пересчёта: суммы, которые
// уходят в сообщение, известны только когда они посчитаны. Плата за это —
// теоретическое «подытожили, а сообщение не ушло» при падении между
// двумя транзакциями; для уведомления в чат это дешевле, чем считать
// суммы дважды ради общей транзакции.
func (r *Repo) EmitPeriodClosed(ctx context.Context, p ProjectPeriod, videos int, views, total int64) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var title string
	if err := tx.QueryRow(ctx, `SELECT title FROM projects WHERE id = $1`, p.ProjectID).Scan(&title); err != nil {
		return fmt.Errorf("project title: %w", err)
	}
	payload := map[string]any{
		"project_id":      p.ProjectID.String(),
		"title":           title,
		"period_seq":      p.Seq,
		"starts_on":       p.StartsOn.Format("2006-01-02"),
		"ends_on":         p.EndsOn.Format("2006-01-02"),
		"videos":          videos,
		"views":           views,
		"total":           total,
		"snapshot_approx": p.SnapshotApprox,
	}
	if p.SnapshotAsOf != nil {
		payload["snapshot_as_of"] = p.SnapshotAsOf.Format("2006-01-02")
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, p.ProjectID.String(),
		outbox.EventProjectPeriodClosed, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- сервис ----

// Periods — периоды проекта. Материализует недостающие: список
// спрашивают с экрана, и «периода нет, пока не сработал тикер» было бы
// враньём про данные, которые уже есть.
func (s *Service) Periods(ctx context.Context, projectID uuid.UUID, now time.Time) ([]ProjectPeriod, error) {
	if err := s.repo.EnsurePeriods(ctx, projectID, now); err != nil {
		return nil, err
	}
	return s.repo.Periods(ctx, projectID)
}

// Period — период проекта: по номеру, а при seq <= 0 — текущий.
//
// Текущий — тот, которому принадлежит сегодняшний день; если проект
// давно не выкладывал, это последний заведённый. ErrNoPeriods означает,
// что не вышло ещё ни одного ролика, и отсчитывать не от чего.
func (s *Service) Period(ctx context.Context, projectID uuid.UUID, seq int, now time.Time) (ProjectPeriod, error) {
	if err := s.repo.EnsurePeriods(ctx, projectID, now); err != nil {
		return ProjectPeriod{}, err
	}
	if seq > 0 {
		return s.repo.PeriodBySeq(ctx, projectID, seq)
	}
	return s.repo.PeriodOn(ctx, projectID, now)
}

// LockPeriod — подытожить период: снять срез на отсечку, пересчитать
// суммы уже по нему и сказать об этом в чат.
//
// Порядок именно такой. Пересчёт идёт ПОСЛЕ подытога, потому что после
// него periodFacts читает числа из среза: так сохранённые суммы и срез
// заведомо об одном и том же. Пересчитай до — и суммы посчитались бы по
// живым просмотрам, то есть по числам, которых на отсечку не было.
//
// Утверждённые и выплаченные строки пересчёт по-прежнему не трогает.
//
// actor nil = фоновая задача; ручного подытога нет.
func (s *Service) LockPeriod(ctx context.Context, p ProjectPeriod, actor *uuid.UUID, asOf, now time.Time) (ProjectPeriod, error) {
	if p.IsLocked() {
		return p, nil
	}
	locked, err := s.repo.LockPeriod(ctx, p.ID, actor, asOf, now)
	if err != nil {
		return ProjectPeriod{}, err
	}
	if !locked.SnapshotApprox {
		// Опереться есть на что — пересчитываем по срезу.
		if _, err := s.Recalculate(ctx, locked.ProjectID, locked); err != nil {
			return locked, err
		}
	}
	// Сообщение в чат — по посчитанному. Ошибку не поднимаем выше:
	// подытог уже состоялся, и откатывать его из-за неотправленного
	// уведомления неправильно.
	if err := s.announcePeriod(ctx, locked); err != nil {
		return locked, fmt.Errorf("period closed announce: %w", err)
	}
	return locked, nil
}

// announcePeriod — событие «период подытожен» с числами периода.
func (s *Service) announcePeriod(ctx context.Context, p ProjectPeriod) error {
	terms, err := s.repo.Terms(ctx, p.ProjectID)
	if err != nil {
		return err
	}
	accruals, err := s.accrualsOrPreview(ctx, p.ProjectID, p, terms)
	if err != nil {
		return err
	}
	t := totals(accruals)
	return s.repo.EmitPeriodClosed(ctx, p, t.VideosDelivered, t.Views, t.Total)
}

// UnlockPeriod — вернуть период в работу (только админ, со следом в
// журнале). См. Repo.UnlockPeriod о последствиях для переноса остатка.
func (s *Service) UnlockPeriod(ctx context.Context, periodID uuid.UUID, actor uuid.UUID, reason string) (ProjectPeriod, error) {
	return s.repo.UnlockPeriod(ctx, periodID, actor, reason)
}

// LockDuePeriods — фоновой подытог: закрыть все периоды, которым пора.
//
// Сначала материализуем периоды у проектов, где что-то вышло: пока
// периода нет строкой, закрывать нечего. Ошибка на одном периоде не
// должна ронять весь проход — остальные проекты в этом не виноваты, а
// следующий тик попробует снова.
func (s *Service) LockDuePeriods(ctx context.Context, now time.Time, delay time.Duration) (locked int, failed int, err error) {
	if delay <= 0 {
		delay = DefaultPeriodLockDelay
	}
	projects, err := s.repo.ProjectsWithPublications(ctx, 0)
	if err != nil {
		return 0, 0, err
	}
	for _, pid := range projects {
		if eerr := s.repo.EnsurePeriods(ctx, pid, now); eerr != nil {
			failed++
		}
	}
	due, err := s.repo.DueForLock(ctx, now, delay, 100)
	if err != nil {
		return 0, failed, err
	}
	for _, p := range due {
		// Отсечка считается от самого периода, а не от «сейчас»:
		// опоздание воркера не должно менять числа, которые период
		// запомнит.
		if _, lerr := s.LockPeriod(ctx, p, nil, p.LockDueAt(delay), now); lerr != nil {
			failed++
			continue
		}
		locked++
	}
	return locked, failed, nil
}
