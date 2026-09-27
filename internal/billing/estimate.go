package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Во сколько обойдётся заказ.
//
// Оклады известны точно — это ставка, умноженная на число креаторов.
// Бонус известен быть не может: он зависит от просмотров, которых ещё
// нет. Поэтому здесь именно ПРОГНОЗ, и считается он по истории тех
// самых людей, что в подборке: сколько их прошлые ролики набирали в
// среднем. Без истории прогноза нет, и молчаливый ноль вместо него был
// бы хуже честного «не знаем».

// CreatorForecast — что известно про одного человека из подборки.
type CreatorForecast struct {
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	// AvgViewsPerVideo — среднее по его сданным роликам, суммарно по пяти
	// площадкам. 0, если сданных роликов ещё не было.
	AvgViewsPerVideo int64 `json:"avg_views_per_video"`
	// BasedOnVideos — на скольких роликах основано среднее. Ноль означает
	// «истории нет», и это видно в интерфейсе, а не прячется в нуле выше.
	BasedOnVideos int `json:"based_on_videos"`
}

// OrderEstimate — смета заказа.
type OrderEstimate struct {
	// Terms — версия правил, с которой клиент согласился при создании
	// заказа. Не действующая на сегодня: цену ему называли по той.
	//
	// Сторона клиента, а не полный тариф: смета — клиентская ручка, а
	// LatestTerms (смета до заказа) читает и креаторские ставки. С
	// общим типом они уезжали заказчику вместе с ценой.
	Terms SideTerms `json:"terms"`
	// Creators/Videos — из самого заказа.
	Creators int `json:"creators"`
	Videos   int `json:"videos"`
	// Salaries — фиксированная часть: фикс за ролик, умноженный на объём
	// (у старых версий прайса — оклад за период на число людей). Точно
	// известна, пока известен объём: бонус за просмотры — прогноз, фикс —
	// нет. Имя поля осталось прежним, чтобы не ломать клиентов.
	Salaries int64 `json:"salaries"`

	// AvgViewsPerVideo — среднее по подборке, из него и растёт прогноз.
	AvgViewsPerVideo int64 `json:"avg_views_per_video"`
	// ViewsForecast — сколько просмотров ожидаем со всего заказа.
	ViewsForecast int64 `json:"views_forecast"`
	// BonusForecast — бонус по ступеням тарифа. Считается НА РОЛИК, а не
	// на общий объём: порог стоит на ролике, и посчитанный по сумме
	// прогноз завышал бы сверхпороговую часть.
	BonusForecast int64 `json:"bonus_forecast"`
	// Total — оклады плюс прогноз бонуса.
	Total int64 `json:"total"`

	// Forecast — разбивка по людям.
	Forecast []CreatorForecast `json:"forecast"`
	// WithoutHistory — по скольким из подборки истории нет. Пока их
	// много, прогноз опирается на меньшинство, и это надо показать.
	WithoutHistory int `json:"without_history"`
	// HasForecast — прогноз посчитан хоть по кому-то. Если false, показывать
	// нужно только оклады: бонус не «ноль», а неизвестен.
	HasForecast bool `json:"has_forecast"`
}

// orderFacts — заказ и его условия.
//
// Читаем ТЕ ЖЕ поля, по которым считается черновая смета (Repo.LatestTerms):
// фикс за ролик и лесенку в том числе. Пока их здесь не было, одно и то
// же число считалось двумя разными механиками — до оформления по фиксу
// за ролик, после оформления по окладу за месяц, — и человек видел на
// странице подбора одну сумму, а в заказе другую. Разойтись они могут
// только молча: обе ветки честно считают, просто по разным условиям.
func (r *Repo) orderFacts(ctx context.Context, orderID, clientID uuid.UUID) (int, int, Terms, error) {
	var needed, videos int
	var t Terms
	err := r.db.QueryRow(ctx, `
SELECT o.needed, o.videos_count,
       v.id, v.salary_per_month, v.rate_per_1000_views, v.bonus_views_threshold,
       v.rate_per_1000_views_over, v.click_bonus_rate,
       v.click_bonus_threshold, v.click_bonus_rate_over,
       v.videos_first_month, v.videos_next_months,
       v.fee_per_video, v.creator_fee_per_video, v.subscriber_rate
FROM creator_orders o
JOIN terms_versions v ON v.id = o.terms_version_id
WHERE o.id = $1 AND o.client_user_id = $2`, orderID, clientID).
		Scan(&needed, &videos, &t.TermsVersionID, &t.SalaryPerMonth,
			&t.RatePer1000Views, &t.BonusViewsThreshold, &t.RatePer1000ViewsOver,
			&t.ClickBonusRate, &t.ClickBonusThreshold, &t.ClickBonusRateOver,
			&t.VideosFirstMonth, &t.VideosNextMonths,
			&t.FeePerVideo, &t.CreatorFeePerVideo, &t.SubscriberRate)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, Terms{}, ErrNotFound
	}
	if err != nil {
		return 0, 0, Terms{}, fmt.Errorf("load order facts: %w", err)
	}
	// Лесенка — часть условий, а не приложение к ним: смета отдаёт её
	// клиенту в terms.steps, и заказ без неё показывал бы ступенчатый
	// тариф плоским.
	if t.TermsVersionID != nil {
		if t.Steps, err = loadSteps(ctx, r.db, stepsOwnerVersion, *t.TermsVersionID); err != nil {
			return 0, 0, Terms{}, err
		}
	}
	return needed, videos, t, nil
}

// creatorsHistory — средние просмотры по произвольному списку людей.
// Нужен смете, которую клиент видит ДО создания заказа: он собирает
// состав, и сумма пересчитывается на каждое изменение.
func (r *Repo) creatorsHistory(ctx context.Context, ids []uuid.UUID) ([]CreatorForecast, error) {
	return r.historyBy(ctx, `
SELECT u.id, `+nameExpr+`, COALESCE(AVG(pub.views), 0)::bigint, COUNT(pub.id)
FROM unnest($1::uuid[]) WITH ORDINALITY AS sel(id, ord)
JOIN users u ON u.id = sel.id
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp     ON cp.user_id = u.id
LEFT JOIN pub ON pub.creator_user_id = u.id
GROUP BY u.id, 2, sel.ord
ORDER BY sel.ord`, ids)
}

// shortlistHistory — средние просмотры по каждому из подборки.
//
// Считаем по сданным выкладкам ЛЮБЫХ проектов: человек, отработавший
// три проекта, приносит в прогноз всё, что о нём известно. Отменённые и
// неcданные не в счёт — они ничего не набрали не потому, что плохие.
func (r *Repo) shortlistHistory(ctx context.Context, orderID uuid.UUID) ([]CreatorForecast, error) {
	return r.historyBy(ctx, `
SELECT c.creator_user_id, `+nameExpr+`,
       COALESCE(AVG(pub.views), 0)::bigint, COUNT(pub.id)
FROM order_candidates c
LEFT JOIN users u                ON u.id = c.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = c.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = c.creator_user_id
LEFT JOIN pub ON pub.creator_user_id = c.creator_user_id
WHERE c.order_id = $1 AND c.status <> 'declined'
GROUP BY c.creator_user_id, 2, c.priority
ORDER BY c.priority`, orderID)
}

// pubHistory — общая часть обоих запросов: средние просмотры по сданным
// роликам, суммой по пяти площадкам на ролик. Вынесена, чтобы смета до
// заказа и смета по заказу считались ОДИНАКОВО: две копии этого CTE
// разъехались бы на первой правке, и клиент увидел бы два разных числа
// до и после создания заказа.
const pubHistory = `
WITH pub AS (
    SELECT p.creator_user_id, p.id, COALESCE(SUM(cur.views), 0) AS views
    FROM project_publications p
    JOIN publication_links l ON l.publication_id = p.id
    LEFT JOIN LATERAL (
        SELECT views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE p.status IN ('done', 'closed_manually')
    GROUP BY p.creator_user_id, p.id
)
`

func (r *Repo) historyBy(ctx context.Context, tail string, args ...any) ([]CreatorForecast, error) {
	rows, err := r.db.Query(ctx, pubHistory+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("shortlist history: %w", err)
	}
	defer rows.Close()
	out := make([]CreatorForecast, 0)
	for rows.Next() {
		var f CreatorForecast
		if err := rows.Scan(&f.CreatorUserID, &f.CreatorName,
			&f.AvgViewsPerVideo, &f.BasedOnVideos); err != nil {
			return nil, fmt.Errorf("scan forecast: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EstimateOrder — смета уже созданного заказа.
func (s *Service) EstimateOrder(ctx context.Context, orderID, clientID uuid.UUID) (OrderEstimate, error) {
	needed, videos, terms, err := s.repo.orderFacts(ctx, orderID, clientID)
	if err != nil {
		return OrderEstimate{}, err
	}
	forecast, err := s.repo.shortlistHistory(ctx, orderID)
	if err != nil {
		return OrderEstimate{}, err
	}
	return assemble(terms, needed, videos, forecast), nil
}

// EstimateDraft — смета состава, который ещё собирают.
//
// По требованию сумма считается НА СТРАНИЦЕ и пересчитывается при каждом
// изменении состава или объёма — то есть до того, как заказ существует.
// Смета по созданному заказу для этого не годится: она отвечает уже
// после того, как всё решено.
//
// Условия берём действующие: именно с ними клиент и согласится, нажав
// «оформить». После создания заказа смета считается по версии, записанной
// в заказ, — числа совпадут, если прайс за эти минуты не поменяли.
func (s *Service) EstimateDraft(ctx context.Context, needed, videos int, creatorIDs []uuid.UUID) (OrderEstimate, error) {
	// Ноль людей — законный вопрос, а не ошибка ввода.
	//
	// Во второй ветке воронки («видео под ключ») креаторов нет вовсе:
	// снимаем мы, ролики выходят с аккаунтов бренда. Цена там —
	// ролики × фикс, и число людей в ней не участвует. Отказ на такой
	// вопрос означал бы пустое место там, где сервер отвечает точно.
	if needed < 0 {
		return OrderEstimate{}, fmt.Errorf("%w: needed must not be negative", ErrInvalidInput)
	}
	if videos < 0 {
		return OrderEstimate{}, fmt.Errorf("%w: videos_count must not be negative", ErrInvalidInput)
	}
	if len(creatorIDs) > 50 {
		return OrderEstimate{}, fmt.Errorf("%w: too many creators", ErrInvalidInput)
	}
	terms, err := s.repo.LatestTerms(ctx)
	if err != nil {
		return OrderEstimate{}, err
	}
	// Состав и объём здесь НЕОБЯЗАТЕЛЬНЫ, и это не послабление.
	//
	// Смету спрашивают раньше, чем набран состав: ползунок «сколько
	// будет стоить следующий месяц» знает только число людей, а экран
	// каталога — людей, но не объём роликов. Отказ на такой вопрос
	// означает пустое место там, где сервер МОЖЕТ ответить точно:
	// оклады считаются из числа людей и ставки, и это уже ответ.
	//
	// Чего не знаем, о том говорим прямо: без состава или без объёма
	// прогноза бонуса нет, и в ответе стоит has_forecast=false — «бонус
	// неизвестен», а не «бонус ноль».
	var forecast []CreatorForecast
	if len(creatorIDs) > 0 {
		forecast, err = s.repo.creatorsHistory(ctx, creatorIDs)
		if err != nil {
			return OrderEstimate{}, err
		}
	}
	return assemble(terms, needed, videos, forecast), nil
}

// assemble — общая сборка сметы для обоих путей.
func assemble(terms Terms, needed, videos int, forecast []CreatorForecast) OrderEstimate {
	e := OrderEstimate{
		Terms:    terms.ClientTerms(),
		Creators: needed,
		Videos:   videos,
		Salaries: fixPart(terms, needed, videos),
		Forecast: forecast,
	}

	// Среднее — только по тем, у кого история есть. Считать человека без
	// истории нулём значит занизить прогноз ровно настолько, насколько
	// подборка новая.
	var sum int64
	var withHistory int
	for _, f := range forecast {
		if f.BasedOnVideos == 0 {
			e.WithoutHistory++
			continue
		}
		sum += f.AvgViewsPerVideo
		withHistory++
	}
	// Прогноза нет — ни по кому не из чего считать, либо не задан объём.
	// Фикс при этом известен точно, его и показываем; has_forecast
	// остаётся false, чтобы ноль бонуса нельзя было прочитать как
	// посчитанный ноль.
	if withHistory == 0 || videos < 1 {
		e.Total = e.Salaries
		return e
	}

	e.HasForecast = true
	e.AvgViewsPerVideo = sum / int64(withHistory)
	e.ViewsForecast = e.AvgViewsPerVideo * int64(videos)
	e.BonusForecast = int64(videos) * bonusForOneVideo(terms, e.AvgViewsPerVideo)
	e.Total = e.Salaries + e.BonusForecast
	return e
}

// fixPart — фиксированная часть сметы.
//
// Фикс считается ЗА РОЛИК: месяц с пятью роликами не должен стоить как
// месяц с тридцатью, и умножать его на число людей — значит назвать цену
// по выключенной механике. Оклад за период остаётся только у старых
// версий прайса, где фикса за ролик нет вовсе.
//
// Без объёма фикс за ролик неизвестен, и ноль здесь — это «не из чего
// считать», ровно как has_forecast=false для бонуса.
func fixPart(t Terms, needed, videos int) int64 {
	if t.FeePerVideo != nil && *t.FeePerVideo > 0 {
		return int64(videos) * *t.FeePerVideo
	}
	return int64(needed) * t.SalaryPerMonth
}

// bonusForOneVideo — бонус за один ролик с такими просмотрами, по
// ступеням тарифа. Отдельной функцией, потому что порог стоит на ролике:
// посчитанный по общему объёму прогноз завышал бы сверхпороговую часть
// (десять роликов по 600 000 — это ноль сверх порога, а не 5 000 000).
func bonusForOneVideo(t Terms, views int64) int64 {
	base, over := views, int64(0)
	if t.BonusViewsThreshold > 0 && views > t.BonusViewsThreshold {
		base = t.BonusViewsThreshold
		over = views - t.BonusViewsThreshold
	}
	return base/1000*t.RatePer1000Views + over/1000*t.RatePer1000ViewsOver
}
