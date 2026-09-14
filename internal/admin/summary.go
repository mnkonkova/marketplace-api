package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Сводка админа: что требует внимания прямо сейчас.
//
// До сих пор это собиралось на фронте из четырёх списков: он тянул все
// проекты, всех пользователей и очередь модерации и считал пересечения у
// себя. Кроме объёма данных, у такого счёта нет главного — единого
// определения. «Без движения 7 дней» на одном экране считалось от
// updated_at, на другом от последнего события, и числа не сходились.
//
// Поэтому считаем на сервере, одним набором правил, и отдаём пункты
// всегда — даже пустые. Пункт, исчезающий при нуле, фронт не может
// отличить от пункта, который сервер забыл посчитать: в первом случае
// надо нарисовать «всё чисто», во втором — не рисовать ничего.

// attentionPreview — сколько строк показываем в пункте. Список здесь
// вспомогательный: он объясняет число («что именно висит?»), а работать
// идут в полноценный список с фильтром.
const attentionPreview = 5

// AttentionItem — одна строка в пункте «требует внимания».
type AttentionItem struct {
	// ID — проект или пользователь, смотря по пункту. Фронт по нему
	// открывает карточку.
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	// Note — чем строка провинилась: «12 дн. без движения», «правок 5
	// из 2». Пусто, если название уже всё сказало.
	Note string `json:"note,omitempty"`
}

// AttentionBlock — пункт сводки: число и короткий список.
type AttentionBlock struct {
	Count int `json:"count"`
	// Items — первые несколько строк, не весь набор. Никогда не null:
	// пустой список фронт рисует как «чисто», отсутствующий — никак.
	Items []AttentionItem `json:"items"`
}

// ModerationBlock — модерация, у которой есть второе число: сколько
// заявок ждут дольше суток. Сутки — обещанный специалистам срок ответа,
// и общее «в очереди 12» без него не говорит, нарушен он или нет.
type ModerationBlock struct {
	AttentionBlock
	OverDay int `json:"over_day"`
}

// Attention — все пункты «требует внимания». Поля, а не map: набор
// пунктов закрытый, и фронт рисует каждый по-своему.
type Attention struct {
	// Moderation — специалисты, ждущие решения о публикации.
	Moderation ModerationBlock `json:"moderation"`
	// ProjectsUnassigned — проекты без ответственного менеджера.
	ProjectsUnassigned AttentionBlock `json:"projects_unassigned"`
	// ProjectsStale — идущие проекты, которые не двигались неделю.
	ProjectsStale AttentionBlock `json:"projects_stale"`
	// ManagersUnapproved — менеджеры, которым не выдали доступ.
	ManagersUnapproved AttentionBlock `json:"managers_unapproved"`
	// SpecialistNotConfirmed — клиент выбрал специалиста в брифе, а
	// менеджер не подтвердил: работа не начнётся, пока не подтвердят.
	SpecialistNotConfirmed AttentionBlock `json:"specialist_not_confirmed"`
	// PublicationsOverdue — выкладки, у которых прошла дата.
	PublicationsOverdue AttentionBlock `json:"publications_overdue"`
	// WorkWithoutPrepayment — проект в работе, а предоплата не
	// подтверждена. Самый дорогой пункт списка: работу уже делают.
	WorkWithoutPrepayment AttentionBlock `json:"work_without_prepayment"`
	// RevisionsExceeded — правок использовано больше, чем включено в
	// условия: дальше либо доплата, либо разговор с клиентом.
	RevisionsExceeded AttentionBlock `json:"revisions_exceeded"`
}

// NavCounts — цифры у разделов бокового меню.
//
// Восемь счётчиков одним блоком, а не восемью ручками: их рисуют на
// каждом заходе в CRM, и семь лишних запросов к API ради приглушённых
// чисел рядом с пунктами меню — это семь лишних запросов на каждый заход.
//
// Ключи присутствуют всегда, включая нулевые: по той же причине, что и у
// распределений — пропавший счётчик читается как сбой, а не как ноль.
// Тестовые проекты и пользователи не в счёт нигде.
type NavCounts struct {
	// ProjectsActive — незавершённые и неотменённые проекты.
	//
	// Внимание: список /admin/projects по умолчанию прячет только
	// отменённые, а завершённые показывает, поэтому его total бывает
	// больше этого числа ровно на количество done. Здесь намеренно
	// «сколько в работе» — цифра у пункта меню отвечает на этот вопрос,
	// а не «сколько строк в таблице».
	ProjectsActive int `json:"projects_active"`
	// ModerationPending — заявки специалистов, ждущие решения. Это число
	// фронт красит отдельно: оно означает, что кого-то держат в очереди.
	ModerationPending int `json:"moderation_pending"`
	// Team — админы и менеджеры вместе, как в /admin/team.
	Team int `json:"team"`
	// Specialists/Clients — по users.kind, как в фильтре /admin/users.
	// Пользователь с kind='both' не попадает ни в один из двух: фильтр
	// списка тоже сверяет kind точным совпадением, и счётчик обязан
	// сходиться со списком, который откроют по клику.
	Specialists int `json:"specialists"`
	Clients     int `json:"clients"`
	// Checklists/Pipelines/Productions — только действующие. Архивные
	// версии шаблонов и выключенные воронки в меню не считаем: их нельзя
	// подключить, и цифра у пункта означала бы объём мусора.
	Checklists  int `json:"checklists"`
	Pipelines   int `json:"pipelines"`
	Productions int `json:"productions"`
}

// Summary — ответ /admin/summary.
type Summary struct {
	Attention Attention `json:"attention"`
	// NavCounts — счётчики у пунктов бокового меню.
	NavCounts NavCounts `json:"nav_counts"`
	// ProjectsByKind/ProjectsByStatus — распределение всех нетестовых
	// проектов. Ключи присутствуют всегда, включая нулевые: иначе
	// пропавший столбец диаграммы читается как сбой, а не как ноль.
	ProjectsByKind   map[string]int `json:"projects_by_kind"`
	ProjectsByStatus map[string]int `json:"projects_by_status"`
	// Managers — нагрузка команды: те же числа, что в /admin/team.
	Managers []TeamMember `json:"managers"`
	// GeneratedAt — момент расчёта. Сводку держат открытой часами, и без
	// отметки непонятно, насколько она устарела.
	GeneratedAt time.Time `json:"generated_at"`
}

// activeStatusesSQL — «работа идёт». Те же статусы, что считает
// нагрузка менеджеров и проверка при снятии роли.
const activeStatusesSQL = `('draft','active','on_hold','dispute')`

// attentionBlock — выполнить запрос пункта.
//
// Запрос обязан вернуть (id, title, note, COUNT(*) OVER ()) и сам
// ограничить выдачу: окно считает по всему набору ДО LIMIT, поэтому
// общее число получается тем же запросом, что и превью, — без второго
// прохода по тем же строкам.
func (r *Repo) attentionBlock(ctx context.Context, name, q string, args ...any) (AttentionBlock, error) {
	block := AttentionBlock{Items: []AttentionItem{}}
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return block, fmt.Errorf("summary %s: %w", name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var it AttentionItem
		if err := rows.Scan(&it.ID, &it.Title, &it.Note, &block.Count); err != nil {
			return block, fmt.Errorf("scan summary %s: %w", name, err)
		}
		block.Items = append(block.Items, it)
	}
	return block, rows.Err()
}

// Summary — всё «требует внимания» плюс распределения и нагрузка.
//
// Запросов много, но каждый узкий и по индексу; одним запросом это было
// бы восемь несвязанных подсчётов в одной строке — не читается и не
// правится. Тестовые проекты и пользователи не учитываются нигде.
func (r *Repo) Summary(ctx context.Context) (Summary, error) {
	out := Summary{
		ProjectsByKind:   map[string]int{},
		ProjectsByStatus: map[string]int{},
		Managers:         []TeamMember{},
		GeneratedAt:      time.Now(),
	}

	// Модерация: очередь и сколько в ней просрочено по суточному сроку.
	mod, err := r.attentionBlock(ctx, "moderation", fmt.Sprintf(`
SELECT sp.user_id,
       COALESCE(NULLIF(sp.display_name, ''), u.email::text, 'без имени'),
       (now()::date - sp.updated_at::date)::text || ' дн. в очереди',
       COUNT(*) OVER ()
FROM specialist_profiles sp
JOIN users u ON u.id = sp.user_id
WHERE sp.moderation_status = 'pending_review' AND sp.is_published = TRUE
  AND u.is_test = FALSE
ORDER BY sp.updated_at ASC
LIMIT %d`, attentionPreview))
	if err != nil {
		return Summary{}, err
	}
	out.Attention.Moderation = ModerationBlock{AttentionBlock: mod}
	if err := r.db.QueryRow(ctx, `
SELECT COUNT(*)
FROM specialist_profiles sp
JOIN users u ON u.id = sp.user_id
WHERE sp.moderation_status = 'pending_review' AND sp.is_published = TRUE
  AND u.is_test = FALSE
  AND sp.updated_at < now() - interval '1 day'`).Scan(&out.Attention.Moderation.OverDay); err != nil {
		return Summary{}, fmt.Errorf("summary moderation over day: %w", err)
	}

	if out.Attention.ProjectsUnassigned, err = r.attentionBlock(ctx, "unassigned", fmt.Sprintf(`
SELECT p.id, p.title,
       (now()::date - p.created_at::date)::text || ' дн. без ответственного',
       COUNT(*) OVER ()
FROM projects p
WHERE p.assigned_to_user_id IS NULL AND p.is_test = FALSE
  AND p.status IN %s
ORDER BY p.created_at ASC
LIMIT %d`, activeStatusesSQL, attentionPreview)); err != nil {
		return Summary{}, err
	}

	if out.Attention.ProjectsStale, err = r.attentionBlock(ctx, "stale", fmt.Sprintf(`
SELECT p.id, p.title,
       (now()::date - p.updated_at::date)::text || ' дн. без движения',
       COUNT(*) OVER ()
FROM projects p
WHERE p.is_test = FALSE AND p.status IN %s
  AND p.updated_at < now() - interval '7 days'
ORDER BY p.updated_at ASC
LIMIT %d`, activeStatusesSQL, attentionPreview)); err != nil {
		return Summary{}, err
	}

	if out.Attention.ManagersUnapproved, err = r.attentionBlock(ctx, "managers_unapproved", fmt.Sprintf(`
SELECT u.id,
       COALESCE(NULLIF(u.display_name, ''), NULLIF(sp.display_name, ''), u.email::text, 'без имени'),
       (now()::date - u.created_at::date)::text || ' дн. ждёт доступа',
       COUNT(*) OVER ()
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE u.is_manager = TRUE AND u.is_approved = FALSE AND u.is_test = FALSE
ORDER BY u.created_at ASC
LIMIT %d`, attentionPreview)); err != nil {
		return Summary{}, err
	}

	if out.Attention.SpecialistNotConfirmed, err = r.attentionBlock(ctx, "specialist_not_confirmed", fmt.Sprintf(`
SELECT p.id, p.title,
       COALESCE(NULLIF(sp.display_name, ''), 'специалист') || ' ждёт подтверждения',
       COUNT(*) OVER ()
FROM projects p
LEFT JOIN specialist_profiles sp ON sp.user_id = p.lead_recipient_specialist_id
WHERE p.is_test = FALSE AND p.status IN %s
  AND p.lead_recipient_specialist_id IS NOT NULL
  AND p.specialist_user_id IS NULL
ORDER BY p.updated_at ASC
LIMIT %d`, activeStatusesSQL, attentionPreview)); err != nil {
		return Summary{}, err
	}

	// Выкладки считаем поштучно, а показываем с названием проекта:
	// «три просроченных» на одном проекте и на трёх — разные новости,
	// и по строкам это видно.
	if out.Attention.PublicationsOverdue, err = r.attentionBlock(ctx, "publications_overdue", fmt.Sprintf(`
SELECT p.id, p.title,
       'выкладка от ' || to_char(pub.due_date, 'DD.MM'),
       COUNT(*) OVER ()
FROM project_publications pub
JOIN projects p ON p.id = pub.project_id
WHERE p.is_test = FALSE
  AND pub.status IN ('planned','partial')
  AND pub.due_date < CURRENT_DATE
ORDER BY pub.due_date ASC
LIMIT %d`, attentionPreview)); err != nil {
		return Summary{}, err
	}

	if out.Attention.WorkWithoutPrepayment, err = r.attentionBlock(ctx, "work_without_prepayment", fmt.Sprintf(`
SELECT p.id, p.title,
       'в работе ' || (now()::date - COALESCE(p.started_at, p.created_at)::date)::text || ' дн.',
       COUNT(*) OVER ()
FROM projects p
WHERE p.is_test = FALSE AND p.status = 'active'
  AND NOT EXISTS (
        SELECT 1 FROM project_payments pay
        WHERE pay.project_id = p.id
          AND pay.kind = 'prepayment' AND pay.status = 'confirmed')
ORDER BY COALESCE(p.started_at, p.created_at) ASC
LIMIT %d`, attentionPreview)); err != nil {
		return Summary{}, err
	}

	if out.Attention.RevisionsExceeded, err = r.attentionBlock(ctx, "revisions_exceeded", fmt.Sprintf(`
SELECT p.id, p.title,
       'правок ' || p.revisions_used::text || ' из ' || p.revisions_included::text,
       COUNT(*) OVER ()
FROM projects p
WHERE p.is_test = FALSE AND p.status IN %s
  AND p.revisions_used > p.revisions_included
ORDER BY (p.revisions_used - p.revisions_included) DESC, p.updated_at ASC
LIMIT %d`, activeStatusesSQL, attentionPreview)); err != nil {
		return Summary{}, err
	}

	// Распределения. Нули проставляем сами: чего нет в GROUP BY, того
	// в ответе иначе не будет вовсе, и фронт не отличит «ни одного» от
	// «не посчитали».
	for _, k := range []string{"creators_turnkey", "production_turnkey", "general"} {
		out.ProjectsByKind[k] = 0
	}
	for _, st := range []string{"draft", "active", "on_hold", "done", "cancelled", "dispute"} {
		out.ProjectsByStatus[st] = 0
	}
	rows, err := r.db.Query(ctx, `
SELECT kind::text, status::text, COUNT(*)
FROM projects
WHERE is_test = FALSE
GROUP BY kind, status`)
	if err != nil {
		return Summary{}, fmt.Errorf("summary distribution: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			kind, status string
			n            int
		)
		if err := rows.Scan(&kind, &status, &n); err != nil {
			return Summary{}, fmt.Errorf("scan distribution: %w", err)
		}
		out.ProjectsByKind[kind] += n
		out.ProjectsByStatus[status] += n
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}

	if out.NavCounts, err = r.navCounts(ctx); err != nil {
		return Summary{}, err
	}

	team, err := r.ListTeam(ctx)
	if err != nil {
		return Summary{}, err
	}
	out.Managers = team
	return out, nil
}

// navCounts — восемь счётчиков меню одним запросом.
//
// Скалярные подзапросы в одной строке, а не восемь вызовов подряд и не
// UNION: каждый счёт идёт по своему индексу и стоит копейки, а одна
// строка — это один поход в базу вместо восьми. Собирать их в UNION
// было бы дороже для чтения, чем для планировщика.
func (r *Repo) navCounts(ctx context.Context) (NavCounts, error) {
	var c NavCounts
	err := r.db.QueryRow(ctx, `
SELECT
  (SELECT COUNT(*) FROM projects p
   WHERE p.is_test = FALSE AND p.status IN `+activeStatusesSQL+`),
  (SELECT COUNT(*) FROM specialist_profiles sp
   JOIN users su ON su.id = sp.user_id
   WHERE sp.moderation_status = 'pending_review' AND sp.is_published = TRUE
     AND su.is_test = FALSE),
  (SELECT COUNT(*) FROM users u
   WHERE u.is_test = FALSE AND (u.is_admin = TRUE OR u.is_manager = TRUE)),
  (SELECT COUNT(*) FROM users u WHERE u.is_test = FALSE AND u.kind = 'specialist'),
  (SELECT COUNT(*) FROM users u WHERE u.is_test = FALSE AND u.kind = 'client'),
  (SELECT COUNT(*) FROM checklist_templates t WHERE t.is_active),
  (SELECT COUNT(*) FROM pipelines pl WHERE pl.is_active),
  (SELECT COUNT(*) FROM productions pr WHERE pr.is_active)`).Scan(
		&c.ProjectsActive, &c.ModerationPending, &c.Team,
		&c.Specialists, &c.Clients, &c.Checklists, &c.Pipelines, &c.Productions)
	if err != nil {
		return NavCounts{}, fmt.Errorf("nav counts: %w", err)
	}
	return c, nil
}

// Summary — сводка админа.
func (s *Service) Summary(ctx context.Context) (Summary, error) {
	return s.repo.Summary(ctx)
}

// AdminSummary godoc
// @Summary  Сводка: что требует внимания
// @Description Пункты считаются на сервере по единым правилам и приходят
// @Description всегда, в том числе нулевыми — решает фронт. Тестовые
// @Description проекты и пользователи не учитываются нигде.
// @Description В nav_counts — цифры у пунктов бокового меню: отдельными
// @Description ручками это было бы восемь запросов на каждый заход в CRM.
// @Tags     admin-summary
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} Summary
// @Router   /admin/summary [get]
func (h *Handler) AdminSummary(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.Summary(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
