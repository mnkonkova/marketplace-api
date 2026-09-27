package ratings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/audit"
)

var (
	// ErrNotFound — версии с таким номером нет.
	ErrNotFound = errors.New("rating scale not found")
	// ErrInvalidInput — пороги не складываются в шкалу.
	ErrInvalidInput = errors.New("invalid input")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

const scaleCols = `s.id, s.version, s.published_at, s.published_by, s.note,
       s.version = (SELECT MAX(version) FROM rating_scales),
       (SELECT COUNT(*) FROM projects p WHERE p.rating_scale_id = s.id),
       (SELECT COUNT(*) FROM project_periods pp WHERE pp.rating_scale_id = s.id),
       s.level_medium_from, s.level_good_from, s.level_great_from,
       s.level_hit_from, s.level_viral_from,
       s.typical_video_views,
       s.share_bad_pct, s.share_medium_pct, s.share_good_pct, s.share_great_pct,
       s.window_days, s.min_mature_videos, s.mature_age_days`

func scanScale(row pgx.Row) (Scale, error) {
	var s Scale
	err := row.Scan(&s.ID, &s.Version, &s.PublishedAt, &s.PublishedBy, &s.Note,
		&s.IsCurrent, &s.UsedByProjects, &s.UsedByPeriods,
		&s.Levels.MediumFrom, &s.Levels.GoodFrom, &s.Levels.GreatFrom,
		&s.Levels.HitFrom, &s.Levels.ViralFrom,
		&s.TypicalVideoViews,
		&s.Shares.BadPct, &s.Shares.MediumPct, &s.Shares.GoodPct, &s.Shares.GreatPct,
		&s.Relative.WindowDays, &s.Relative.MinMatureVideos, &s.Relative.MatureAgeDays)
	return s, err
}

// List — все версии, свежие первыми, вместе с площадками и рынком.
func (r *Repo) List(ctx context.Context) ([]Scale, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+scaleCols+` FROM rating_scales s ORDER BY s.version DESC`)
	if err != nil {
		return nil, fmt.Errorf("list rating scales: %w", err)
	}
	defer rows.Close()
	out := make([]Scale, 0, 4)
	ids := make([]uuid.UUID, 0, 4)
	for rows.Next() {
		s, err := scanScale(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rating scale: %w", err)
		}
		out = append(out, s)
		ids = append(ids, s.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.hydrate(ctx, out, ids); err != nil {
		return nil, err
	}
	return out, nil
}

// Current — действующая версия: самая новая.
func (r *Repo) Current(ctx context.Context) (Scale, error) {
	s, err := scanScale(r.db.QueryRow(ctx,
		`SELECT `+scaleCols+` FROM rating_scales s ORDER BY s.version DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Scale{}, ErrNotFound
	}
	if err != nil {
		return Scale{}, fmt.Errorf("current rating scale: %w", err)
	}
	if err := r.hydrate(ctx, []Scale{s}, []uuid.UUID{s.ID}); err != nil {
		return Scale{}, err
	}
	return r.byID(ctx, s.ID)
}

// ByID — конкретная версия. Ею пользуются проект и подытоженный период:
// у них лежит ссылка, а не копия чисел — версия неизменяема, поэтому
// ссылка и есть снимок.
func (r *Repo) ByID(ctx context.Context, id uuid.UUID) (Scale, error) { return r.byID(ctx, id) }

func (r *Repo) byID(ctx context.Context, id uuid.UUID) (Scale, error) {
	s, err := scanScale(r.db.QueryRow(ctx,
		`SELECT `+scaleCols+` FROM rating_scales s WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Scale{}, ErrNotFound
	}
	if err != nil {
		return Scale{}, fmt.Errorf("rating scale by id: %w", err)
	}
	list := []Scale{s}
	if err := r.hydrate(ctx, list, []uuid.UUID{s.ID}); err != nil {
		return Scale{}, err
	}
	return list[0], nil
}

// hydrate — догрузить площадки и рынок одним запросом на весь список.
func (r *Repo) hydrate(ctx context.Context, scales []Scale, ids []uuid.UUID) error {
	if len(scales) == 0 {
		return nil
	}
	byID := make(map[uuid.UUID]*Scale, len(scales))
	for i := range scales {
		// Пустой список — это [], а не null: потребитель, ждущий массив,
		// падает на первой же версии без площадок.
		scales[i].PlatformLevels = []PlatformLevels{}
		scales[i].Market = []MarketPrice{}
		byID[scales[i].ID] = &scales[i]
	}

	prows, err := r.db.Query(ctx, `
SELECT scale_id, platform, medium_from, good_from, great_from
FROM rating_scale_platforms WHERE scale_id = ANY($1)
ORDER BY platform`, ids)
	if err != nil {
		return fmt.Errorf("list scale platforms: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var (
			id uuid.UUID
			p  PlatformLevels
		)
		if err := prows.Scan(&id, &p.Platform, &p.MediumFrom, &p.GoodFrom, &p.GreatFrom); err != nil {
			return fmt.Errorf("scan scale platform: %w", err)
		}
		if s := byID[id]; s != nil {
			s.PlatformLevels = append(s.PlatformLevels, p)
		}
	}
	if err := prows.Err(); err != nil {
		return err
	}

	mrows, err := r.db.Query(ctx, `
SELECT scale_id, key, title, price_per_1000, source, measured_on
FROM rating_scale_market WHERE scale_id = ANY($1)
ORDER BY key`, ids)
	if err != nil {
		return fmt.Errorf("list scale market: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			id uuid.UUID
			m  MarketPrice
		)
		if err := mrows.Scan(&id, &m.Key, &m.Title, &m.PricePer1000, &m.Source, &m.MeasuredOn); err != nil {
			return fmt.Errorf("scan scale market: %w", err)
		}
		if s := byID[id]; s != nil {
			s.Market = append(s.Market, m)
		}
	}
	return mrows.Err()
}

// Publish — выпустить новую версию.
//
// Правки существующей нет намеренно: на прежней версии живут проекты и
// подытоженные периоды, и переписать её значит переписать то, что клиент
// уже видел. Всё одной транзакцией вместе с записью в журнал: выпуск
// порогов — админское действие, и след у него такой же обязательный, как
// у выпуска прайса.
func (r *Repo) Publish(ctx context.Context, in Scale, actor uuid.UUID) (PublishResult, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PublishResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Прежняя действующая — в той же транзакции: между «с чем сравнивать»
	// и вставкой мог бы встрять второй выпуск, и разница считалась бы от
	// версии, которая уже не действует.
	prev, havePrev, err := currentInTx(ctx, tx)
	if err != nil {
		return PublishResult{}, err
	}

	var newID uuid.UUID
	var version int
	if err := tx.QueryRow(ctx, `
INSERT INTO rating_scales (
    version, note, published_by,
    level_medium_from, level_good_from, level_great_from, level_hit_from, level_viral_from,
    typical_video_views,
    share_bad_pct, share_medium_pct, share_good_pct, share_great_pct,
    window_days, min_mature_videos, mature_age_days
) VALUES (
    (SELECT COALESCE(MAX(version), 0) + 1 FROM rating_scales),
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
)
RETURNING id, version`,
		in.Note, actorOrNil(actor),
		in.Levels.MediumFrom, in.Levels.GoodFrom, in.Levels.GreatFrom,
		in.Levels.HitFrom, in.Levels.ViralFrom,
		in.TypicalVideoViews,
		in.Shares.BadPct, in.Shares.MediumPct, in.Shares.GoodPct, in.Shares.GreatPct,
		in.Relative.WindowDays, in.Relative.MinMatureVideos, in.Relative.MatureAgeDays).
		Scan(&newID, &version); err != nil {
		return PublishResult{}, fmt.Errorf("insert rating scale: %w", err)
	}

	for _, p := range in.PlatformLevels {
		if _, err := tx.Exec(ctx, `
INSERT INTO rating_scale_platforms (scale_id, platform, medium_from, good_from, great_from)
VALUES ($1, $2, $3, $4, $5)`, newID, p.Platform, p.MediumFrom, p.GoodFrom, p.GreatFrom); err != nil {
			return PublishResult{}, fmt.Errorf("insert scale platform %s: %w", p.Platform, err)
		}
	}
	for _, m := range in.Market {
		if _, err := tx.Exec(ctx, `
INSERT INTO rating_scale_market (scale_id, key, title, price_per_1000, source, measured_on)
VALUES ($1, $2, $3, $4, $5, $6)`,
			newID, m.Key, m.Title, m.PricePer1000, m.Source, m.MeasuredOn); err != nil {
			return PublishResult{}, fmt.Errorf("insert scale market %s: %w", m.Key, err)
		}
	}

	out := PublishResult{Changes: []ScaleChange{}}
	if havePrev {
		out.Changes = diffScales(prev, in)
		// Проекты за новой версией не последуют: копия снимается один
		// раз. Число показываем сразу — оно объясняет, почему на экранах
		// ещё долго будут прежние пороги.
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM projects WHERE rating_scale_id = $1`, prev.ID).
			Scan(&out.ProjectsOnPrevious); err != nil {
			return PublishResult{}, fmt.Errorf("count projects on previous: %w", err)
		}
	}

	if err := audit.Write(ctx, tx, actor, audit.ActionRatingScalePublish,
		audit.ObjectRatingScale, newID.String(), map[string]any{
			"version":              version,
			"changes":              len(out.Changes),
			"projects_on_previous": out.ProjectsOnPrevious,
			"note":                 in.Note,
		}); err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, fmt.Errorf("commit: %w", err)
	}

	scale, err := r.byID(ctx, newID)
	if err != nil {
		return PublishResult{}, err
	}
	out.Scale = scale
	return out, nil
}

func actorOrNil(actor uuid.UUID) any {
	if actor == uuid.Nil {
		return nil
	}
	return actor
}

// currentInTx — действующая версия внутри транзакции, без площадок и
// рынка: для разницы нужны только числа самой шкалы.
func currentInTx(ctx context.Context, tx pgx.Tx) (Scale, bool, error) {
	s, err := scanScale(tx.QueryRow(ctx,
		`SELECT `+scaleCols+` FROM rating_scales s ORDER BY s.version DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Scale{}, false, nil
	}
	if err != nil {
		return Scale{}, false, fmt.Errorf("current scale in tx: %w", err)
	}
	return s, true, nil
}

// AttachToProject — проект снимает копию действующей версии.
//
// Один раз: у проекта, который уже на какой-то версии, она остаётся.
// Иначе выпуск новых порогов перекраивал бы оценки идущих проектов — то
// самое «поправили порог, и прошлое пересчиталось», от чего справочник и
// версионируется.
func (r *Repo) AttachToProject(ctx context.Context, projectID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.QueryRow(ctx, `
UPDATE projects
SET rating_scale_id = (SELECT id FROM rating_scales ORDER BY version DESC LIMIT 1)
WHERE id = $1 AND rating_scale_id IS NULL
RETURNING rating_scale_id`, projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Копия уже снята (или проекта нет) — возвращаем что есть.
		if err := r.db.QueryRow(ctx,
			`SELECT rating_scale_id FROM projects WHERE id = $1`, projectID).Scan(&id); err != nil {
			return uuid.Nil, ErrNotFound
		}
		return id, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("attach rating scale: %w", err)
	}
	return id, nil
}

// ForProject — версия, по которой оценивается проект. Копии ещё нет —
// снимаем её сейчас: первый же вопрос об оценке и есть момент, когда
// проект встаёт на шкалу.
func (r *Repo) ForProject(ctx context.Context, projectID uuid.UUID) (Scale, error) {
	var id *uuid.UUID
	if err := r.db.QueryRow(ctx,
		`SELECT rating_scale_id FROM projects WHERE id = $1`, projectID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Scale{}, ErrNotFound
		}
		return Scale{}, fmt.Errorf("project rating scale: %w", err)
	}
	if id == nil {
		attached, err := r.AttachToProject(ctx, projectID)
		if err != nil {
			return Scale{}, err
		}
		return r.byID(ctx, attached)
	}
	return r.byID(ctx, *id)
}

// diffScales — изменившиеся числа между версиями, в порядке полей формы.
func diffScales(from, to Scale) []ScaleChange {
	out := []ScaleChange{}
	add := func(field, label string, a, b int64) {
		if a == b {
			return
		}
		x, y := a, b
		out = append(out, ScaleChange{Field: field, Label: label, From: &x, To: &y})
	}
	add("level_medium_from", "Средний ролик от", from.Levels.MediumFrom, to.Levels.MediumFrom)
	add("level_good_from", "Хороший ролик от", from.Levels.GoodFrom, to.Levels.GoodFrom)
	add("level_great_from", "Отличный ролик от", from.Levels.GreatFrom, to.Levels.GreatFrom)
	add("level_hit_from", "Хит от", from.Levels.HitFrom, to.Levels.HitFrom)
	add("level_viral_from", "Виральный от", from.Levels.ViralFrom, to.Levels.ViralFrom)
	add("typical_video_views", "Типичный ролик", from.TypicalVideoViews, to.TypicalVideoViews)
	add("share_bad_pct", "Доля плохих, %", int64(from.Shares.BadPct), int64(to.Shares.BadPct))
	add("share_medium_pct", "Доля средних, %", int64(from.Shares.MediumPct), int64(to.Shares.MediumPct))
	add("share_good_pct", "Доля хороших, %", int64(from.Shares.GoodPct), int64(to.Shares.GoodPct))
	add("share_great_pct", "Доля отличных и выше, %", int64(from.Shares.GreatPct), int64(to.Shares.GreatPct))
	add("window_days", "Окно сравнения, дней", int64(from.Relative.WindowDays), int64(to.Relative.WindowDays))
	add("min_mature_videos", "Минимум зрелых роликов", int64(from.Relative.MinMatureVideos), int64(to.Relative.MinMatureVideos))
	add("mature_age_days", "Зрелость ролика, дней", int64(from.Relative.MatureAgeDays), int64(to.Relative.MatureAgeDays))
	return out
}

// StampPeriod — записать периоду версию, которой его оценивали.
//
// Зовётся при подытоге, в его транзакции: пороги замораживаются вместе с
// просмотрами и суммами. Ставится один раз — переоткрытие периода
// отметку не снимает, и это осознанно: чем оценивали, тем и оценивали.
func StampPeriod(ctx context.Context, tx pgx.Tx, periodID, projectID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
UPDATE project_periods
SET rating_scale_id = COALESCE(
    rating_scale_id,
    (SELECT p.rating_scale_id FROM projects p WHERE p.id = $2),
    (SELECT id FROM rating_scales ORDER BY version DESC LIMIT 1)
)
WHERE id = $1`, periodID, projectID); err != nil {
		return fmt.Errorf("stamp period rating scale: %w", err)
	}
	return nil
}
