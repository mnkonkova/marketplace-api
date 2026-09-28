package publications

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// Ручки страницы проекта, которых не хватало: состав с именами, материалы,
// чеклист на стороне менеджера, автопинг и список проектов у креатора.

type personsResp struct {
	Items []Person `json:"items"`
}

type materialsResp struct {
	Items []Material `json:"items"`
}

type templatesResp struct {
	Items []ChecklistTemplate `json:"items"`
}

type creatorProjectsResp struct {
	Items []CreatorProject `json:"items"`
}

type materialReq struct {
	// Kind — doc, video или link.
	Kind  string `json:"kind"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Audience — creators (по умолчанию) или client. Материалы для
	// креаторов заказчик не видит.
	Audience string `json:"audience"`
}

// assertCreator — проект есть и пользователь в его действующем составе.
// Отдельно от managerProject: у креатора нет менеджерских прав, а доступ
// к проекту есть.
func (h *Handler) assertCreator(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return uuid.Nil, uuid.Nil, false
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return uuid.Nil, uuid.Nil, false
	}
	in, err := h.svc.CreatorInProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	if !in {
		// Не 403: «такой проект есть, но не ваш» — это подтверждение
		// существования проекта постороннему.
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return uuid.Nil, uuid.Nil, false
	}
	return projectID, uid, true
}

// ---- состав проекта ----

// ManagerListCreators godoc
// @Summary  Состав проекта (менеджер)
// @Description Действующие креаторы с именами и ссылками на аккаунты по
// @Description пяти площадкам — чтобы проверить выкладку глазами, не
// @Description спрашивая креатора. Выбывшие из состава не показываются.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} personsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/creators [get]
func (h *Handler) ManagerListCreators(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	// Не пустым списком: пустой состав читается как «ещё никого не
	// добавили», а здесь его не будет никогда.
	if !h.requireFeature(w, r, projectID, hasCrew, whyNoCrew) {
		return
	}
	items, err := h.svc.ProjectCreators(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, personsResp{Items: items})
}

// CreatorProjects godoc
// @Summary  Мои проекты (креатор)
// @Description Проекты, где я в действующем составе, со счётчиками только
// @Description по своим выкладкам: сколько всего, сколько не сдано и
// @Description сколько горит. Просьба о переносе снимает просрочку — так же,
// @Description как в напоминаниях.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} creatorProjectsResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/creator/projects [get]
func (h *Handler) CreatorProjects(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	items, err := h.svc.CreatorProjects(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, creatorProjectsResp{Items: items})
}

// ---- материалы ----

// ManagerListMaterials godoc
// @Summary  Материалы проекта (менеджер)
// @Description Все материалы обеих аудиторий. Приложенное к сдаче сюда не
// @Description попадает — оно живёт в карточке сдачи.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} materialsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/materials [get]
func (h *Handler) ManagerListMaterials(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListMaterials(r.Context(), projectID, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, materialsResp{Items: items})
}

// ManagerAddMaterial godoc
// @Summary  Добавить материал в проект (менеджер)
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string      true "project id"
// @Param    body body materialReq true "материал"
// @Success  201  {object} Material
// @Failure  400  {object} errorResponse "bad_json; bad_id; invalid_input — неизвестный kind или audience, пустое название, ссылка не http(s)"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Failure  409  {object} errorResponse "too_many_materials — в проекте уже сто материалов"
// @Router   /manager/projects/{id}/materials [post]
func (h *Handler) ManagerAddMaterial(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req materialReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	m, err := h.svc.AddMaterial(r.Context(), AddMaterialInput{
		ProjectID: projectID, Kind: req.Kind, Title: req.Title,
		URL: req.URL, Audience: req.Audience, CreatedBy: uid,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

// ManagerDeleteMaterial godoc
// @Summary  Убрать материал из проекта (менеджер)
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id          path string true "project id"
// @Param    material_id path string true "material id"
// @Success  204 "удалено"
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — материал не найден или он из другого проекта"
// @Router   /manager/projects/{id}/materials/{material_id} [delete]
func (h *Handler) ManagerDeleteMaterial(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	materialID, err := pathUUID(r, "material_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id материала.")
		return
	}
	if err := h.svc.DeleteMaterial(r.Context(), projectID, materialID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreatorMaterials godoc
// @Summary  Материалы проекта (креатор)
// @Description Бренд-гайд, обучение и прочее, что открывается в момент
// @Description добавления в проект.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} materialsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или вы не в его составе"
// @Router   /me/creator/projects/{id}/materials [get]
func (h *Handler) CreatorMaterials(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.assertCreator(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListMaterials(r.Context(), projectID, AudienceCreators)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, materialsResp{Items: items})
}

type creatorDocumentsResp struct {
	Items []CreatorDocument `json:"items"`
}

// CreatorDocuments godoc
// @Summary  Мои документы (креатор)
// @Description Договоры и документы по всем действующим проектам, в одном
// @Description списке и с названием проекта у каждого. Отдельно от
// @Description материалов проекта: договор ищут не тогда, когда снимают, а
// @Description когда подписывают или выставляют счёт, — и помнить, в каком
// @Description проекте он лежал, человек не обязан. Видео и ссылки сюда не
// @Description попадают: это материалы для работы, а не документы.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} creatorDocumentsResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/creator/documents [get]
func (h *Handler) CreatorDocuments(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	items, err := h.svc.CreatorDocuments(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, creatorDocumentsResp{Items: items})
}

// ClientMaterials godoc
// @Summary  Материалы проекта (заказчик)
// @Description Только помеченные как клиентские. Обучение и бренд-гайд для
// @Description креаторов заказчику не показываются.
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} materialsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или он не ваш"
// @Router   /me/projects/{id}/materials [get]
func (h *Handler) ClientMaterials(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListMaterials(r.Context(), projectID, AudienceClient)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, materialsResp{Items: items})
}

// ---- чеклист ----

// ManagerChecklist godoc
// @Summary  Чеклист проекта (менеджер)
// @Description Снимок, подключённый к проекту. Правки в нём остаются внутри
// @Description проекта: библиотека шаблонов живёт отдельно.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} managerChecklistResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/checklist [get]
func (h *Handler) ManagerChecklist(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	// Пустой чек-лист читается как «забыли подключить». У этого вида его
	// не будет никогда — говорим об этом прямо.
	if !h.requireFeature(w, r, projectID, hasChecklist, whyNoChecklist) {
		return
	}
	items, err := h.svc.ProjectChecklist(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	meta, err := h.svc.ChecklistMeta(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managerChecklistResp{Items: items, Template: meta})
}

// managerChecklistResp — пункты плюс шапка: какой шаблон подключён, какой
// он был версии и не вышла ли с тех пор новая.
type managerChecklistResp struct {
	Items    []ChecklistItem        `json:"items"`
	Template *ChecklistSnapshotMeta `json:"template,omitempty"`
}

// ManagerChecklistTemplates godoc
// @Summary  Библиотека чеклистов (менеджер)
// @Description Из чего выбирать при подключении чеклиста к проекту.
// @Description Выключенные шаблоны не показываются: подключить их нельзя.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} templatesResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /manager/checklist_templates [get]
func (h *Handler) ManagerChecklistTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFrom(r.Context()); !ok {
		writeNoUser(w)
		return
	}
	items, err := h.svc.ChecklistTemplates(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, templatesResp{Items: items})
}

// ---- автопинг ----

type autopingReq struct {
	DueToday      *bool `json:"due_today"`
	Overdue       *bool `json:"overdue"`
	Incomplete    *bool `json:"incomplete"`
	ManagerDigest *bool `json:"manager_digest"`
	DayBefore     *bool `json:"day_before"`
}

// ManagerAutoping godoc
// @Summary  Настройки автопинга проекта (менеджер)
// @Description Четыре выключателя по четырём видам напоминаний. Проект, где
// @Description ничего не трогали, пингуется полностью — отсутствие настроек
// @Description означает «всё включено».
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} ReminderPrefs
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/autoping [get]
func (h *Handler) ManagerAutoping(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	prefs, err := h.svc.ReminderPrefs(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, prefs)
}

// ManagerSaveAutoping godoc
// @Summary  Изменить автопинг проекта (менеджер)
// @Description Поля необязательные: не переданное остаётся как было.
// @Description Выключенное напоминание не откладывается, а не отправляется;
// @Description кнопка «напомнить» руками автопингом не управляется.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string      true "project id"
// @Param    body body autopingReq true "выключатели"
// @Success  200  {object} ReminderPrefs
// @Failure  400  {object} errorResponse "bad_json; bad_id"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/autoping [put]
func (h *Handler) ManagerSaveAutoping(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req autopingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	// Читаем текущее и накладываем переданное: PUT с одним полем не
	// должен молча выключать три остальных.
	prefs, err := h.svc.ReminderPrefs(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.DueToday != nil {
		prefs.DueToday = *req.DueToday
	}
	if req.Overdue != nil {
		prefs.Overdue = *req.Overdue
	}
	if req.Incomplete != nil {
		prefs.Incomplete = *req.Incomplete
	}
	if req.ManagerDigest != nil {
		prefs.ManagerDigest = *req.ManagerDigest
	}
	if req.DayBefore != nil {
		prefs.DayBefore = *req.DayBefore
	}
	saved, err := h.svc.SaveReminderPrefs(r.Context(), prefs, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// CreatorProjectCard godoc
// @Summary  Карточка проекта (креатор)
// @Description Шапка страницы выкладок: название, бриф, период, месячный
// @Description план, нужен ли черновик, кому писать и счётчики по своим
// @Description выкладкам. Раньше всё это фронт собирал из списка выкладок,
// @Description а брифа и плана там взять было неоткуда.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} CreatorProjectCard
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или вы не в его составе"
// @Router   /me/creator/projects/{id} [get]
func (h *Handler) CreatorProjectCard(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	card, err := h.svc.CreatorProjectCard(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, card)
}

// ---- библиотека чеклистов под админом ----

// saveTemplateReq — шаблон целиком. Правки нет: шаблон выпускается
// новой версией, прежняя гасится.
type saveTemplateReq struct {
	// Replaces — id версии, которую заменяем. Пусто — новый шаблон.
	Replaces    string                  `json:"replaces,omitempty"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Items       []ChecklistTemplateItem `json:"items"`
}

// AdminListChecklistTemplates godoc
// @Summary  Библиотека чеклистов (админ)
// @Tags     admin-publications
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} templatesResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  403 {object} errorResponse "forbidden — нужна роль admin"
// @Router   /admin/checklist_templates [get]
func (h *Handler) AdminListChecklistTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ChecklistTemplates(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, templatesResp{Items: items})
}

// AdminGetChecklistTemplate godoc
// @Summary  Шаблон чеклиста целиком (админ)
// @Tags     admin-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "template id"
// @Success  200 {object} ChecklistTemplateFull
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  404 {object} errorResponse "not_found"
// @Router   /admin/checklist_templates/{id} [get]
func (h *Handler) AdminGetChecklistTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id шаблона.")
		return
	}
	out, err := h.svc.ChecklistTemplate(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// AdminSaveChecklistTemplate godoc
// @Summary  Выпустить версию шаблона чеклиста (админ)
// @Description Правки на месте нет намеренно: шаблон уходит в проекты снимком,
// @Description и подмена пунктов под идущими проектами означала бы, что креатор
// @Description отмечал одно, а спросят с него другое. Прежняя версия гасится.
// @Tags     admin-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body saveTemplateReq true "шаблон целиком"
// @Success  201 {object} ChecklistTemplateFull
// @Failure  400 {object} errorResponse "bad_json; invalid_input — пустое название, нет пунктов, неизвестная площадка"
// @Failure  404 {object} errorResponse "not_found — заменяемой версии нет или она уже погашена"
// @Router   /admin/checklist_templates [post]
func (h *Handler) AdminSaveChecklistTemplate(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	var req saveTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	var replaces uuid.UUID
	if s := strings.TrimSpace(req.Replaces); s != "" {
		parsed, perr := uuid.Parse(s)
		if perr != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "replaces должен быть UUID.")
			return
		}
		replaces = parsed
	}
	out, err := h.svc.SaveChecklistTemplate(r.Context(), actor, replaces, req.Name, req.Description, req.Items)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// AdminDeleteChecklistTemplate godoc
// @Summary  Убрать шаблон из библиотеки (админ)
// @Description Проекты, куда он уже подключён, не меняются: у них свой снимок.
// @Tags     admin-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "template id"
// @Success  204
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  404 {object} errorResponse "not_found — шаблона нет или он уже погашен"
// @Router   /admin/checklist_templates/{id} [delete]
func (h *Handler) AdminDeleteChecklistTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id шаблона.")
		return
	}
	if err := h.svc.DeactivateChecklistTemplate(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- переключатели проекта ----

// ManagerProjectSettings godoc
// @Summary  Настройки проекта: этап черновика и показ статистики (менеджер)
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} ProjectSettings
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/settings [get]
func (h *Handler) ManagerProjectSettings(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ProjectSettings(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ManagerSaveProjectSettings godoc
// @Summary  Изменить настройки проекта (менеджер)
// @Description Этап черновика добавляет каждой НОВОЙ выкладке второй срок;
// @Description уже проставленные сроки выключение не стирает — по ним креатор
// @Description уже сдаёт. Показ статистики закрывает заказчику и отчёт, и цифры
// @Description в ленте роликов.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string          true "project id"
// @Param    body body ProjectSettings true "переключатели"
// @Success  200 {object} ProjectSettings
// @Failure  400 {object} errorResponse "bad_json; bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/settings [put]
func (h *Handler) ManagerSaveProjectSettings(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req ProjectSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	out, err := h.svc.SaveProjectSettings(r.Context(), projectID, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- колокольчик «напомнить накануне» ----

type creatorRemindReq struct {
	DayBefore *bool `json:"day_before"`
}

// ManagerSetCreatorReminder godoc
// @Summary  Напоминать креатору накануне срока (менеджер)
// @Description Колокольчик в плане выкладок — напротив каждого креатора
// @Description свой. Сильнее настройки проекта: напоминание включают тому,
// @Description кто забывает, а не всем восьмерым разом. «Завтра срок» —
// @Description ещё рабочее напоминание, в отличие от «сегодня срок».
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id         path string true "project id"
// @Param    creator_id path string true "creator user id"
// @Param    body body creatorRemindReq true "day_before"
// @Success  200 {object} CreatorReminderPref
// @Failure  400 {object} errorResponse "bad_json; bad_id; bad_creator_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/creators/{creator_id}/reminders [put]
func (h *Handler) ManagerSetCreatorReminder(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	creatorID, err := pathUUID(r, "creator_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_creator_id", "Неверный id креатора.")
		return
	}
	var req creatorRemindReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	if req.DayBefore == nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json",
			"Нужно поле day_before: true или false.")
		return
	}
	saved, err := h.svc.SaveCreatorReminderPref(r.Context(), projectID, creatorID, *req.DayBefore, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// ---- «это ваш ролик?» ----

type suggestionsResp struct {
	Items []LinkSuggestion `json:"items"`
}

// CreatorSuggestions godoc
// @Summary  Найденные ролики, ждущие подтверждения (креатор)
// @Description Сервис находит ролик на аккаунте креатора раньше, чем тот
// @Description успевает вставить ссылку, — но ничего не привязывает сам:
// @Description подтверждает человек. К какой выкладке предложить привязку,
// @Description считается при чтении, поэтому перенос срока подсказку не ломает.
// @Description Находки по всем проектам сразу: карточка приходит от площадки,
// @Description а не от проекта.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} suggestionsResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Router   /me/creator/suggestions [get]
func (h *Handler) CreatorSuggestions(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	items, err := h.svc.CreatorSuggestions(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, suggestionsResp{Items: items})
}

type linkSuggestionReq struct {
	// PublicationID — к какой выкладке привязать. Пусто — к той, что
	// предложил сервис.
	PublicationID string `json:"publication_id"`
	// CheckedItemIDs — пункты чеклиста: привязка идёт тем же путём, что
	// и ссылка, вставленная руками, и проверяется так же.
	CheckedItemIDs []uuid.UUID `json:"checked_item_ids"`
}

// CreatorLinkSuggestion godoc
// @Summary  «Да, мой» — привязать найденный ролик (креатор)
// @Description Находка становится обычной сданной ссылкой: те же проверки
// @Description чеклиста, то же закрытие выкладки при пятой площадке.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string true "suggestion id"
// @Param    body body linkSuggestionReq false "куда привязать"
// @Success  200 {object} Publication
// @Failure  400 {object} errorResponse "bad_json; bad_id; invalid_input"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — находка не найдена или чужая"
// @Failure  409 {object} errorResponse "suggestion_decided — по находке уже ответили"
// @Failure  422 {object} errorResponse "checklist_incomplete"
// @Router   /me/creator/suggestions/{id}/link [post]
func (h *Handler) CreatorLinkSuggestion(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id находки.")
		return
	}
	var req linkSuggestionReq
	if r.Body != nil {
		// Тело необязательное: «привязать к предложенной выкладке» —
		// это запрос без единого поля.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
			return
		}
	}
	var pubID uuid.UUID
	if s := strings.TrimSpace(req.PublicationID); s != "" {
		pubID, err = uuid.Parse(s)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id выкладки.")
			return
		}
	}
	pub, err := h.svc.LinkSuggestion(r.Context(), id, uid, pubID, req.CheckedItemIDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pub)
}

// CreatorDismissSuggestion godoc
// @Summary  «Не мой» — отклонить находку (креатор)
// @Description Отказ хранится, а не забывается: обход аккаунта идёт каждый
// @Description день, и удалённая находка вернулась бы завтра той же карточкой.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "suggestion id"
// @Success  204 "отклонено"
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — находка не найдена или чужая"
// @Failure  409 {object} errorResponse "suggestion_decided — по находке уже ответили"
// @Router   /me/creator/suggestions/{id} [delete]
func (h *Handler) CreatorDismissSuggestion(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id находки.")
		return
	}
	if err := h.svc.DismissSuggestion(r.Context(), id, uid); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type addSuggestionReq struct {
	CreatorUserID string `json:"creator_user_id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	AuthorHandle  string `json:"author_handle"`
	// PublishedAt — когда ролик вышел на площадке, RFC3339. Из неё
	// подбирается выкладка, к которой предложить привязку.
	PublishedAt string `json:"published_at"`
}

// ManagerAddSuggestion godoc
// @Summary  Положить найденный ролик (менеджер / обход аккаунтов)
// @Description Точка входа для того, кто находит ролики на площадках: обхода
// @Description аккаунтов, бота или менеджера, наткнувшегося на ролик руками.
// @Description Сама находка ничего не меняет — её подтверждает креатор.
// @Description Повторная находка того же адреса не плодит карточку: обход
// @Description идёт каждый день, а «не мой» не должен возвращаться завтра.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string true "project id"
// @Param    body body addSuggestionReq true "находка"
// @Success  200 {object} LinkSuggestion
// @Failure  400 {object} errorResponse "bad_json; bad_id; unknown_platform"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/link-suggestions [post]
func (h *Handler) ManagerAddSuggestion(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req addSuggestionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	creatorID, err := uuid.Parse(strings.TrimSpace(req.CreatorUserID))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id креатора.")
		return
	}
	in := AddSuggestionInput{
		ProjectID:     projectID,
		CreatorUserID: creatorID,
		URL:           req.URL,
		Title:         req.Title,
		AuthorHandle:  req.AuthorHandle,
	}
	if s := strings.TrimSpace(req.PublishedAt); s != "" {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json",
				"published_at должен быть датой в формате RFC3339.")
			return
		}
		in.PublishedAt = &at
	}
	saved, err := h.svc.AddSuggestion(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// ---- заявка на следующий месяц ----

// monthRequestReq — что прислал кабинет заказчика: ползунки и число,
// которое он видел сверху.
type monthRequestReq struct {
	Creators int `json:"creators"`
	Videos   int `json:"videos"`
	// Ceiling — потолок в копейках, показанный на экране. Пересчитывать
	// его на сервере незачем: разговор пойдёт именно о той сумме,
	// которую человек прочитал, а прайс к этому моменту может смениться.
	Ceiling int64 `json:"ceiling"`
	// Month — «ГГГГ-ММ». Пусто — следующий месяц от сегодняшнего дня.
	Month string `json:"month,omitempty"`
}

type monthRequestResp struct {
	Request *MonthRequest `json:"request"`
}

// ClientAskMonth godoc
// @Summary  Заказать следующий месяц (заказчик)
// @Description Прикидка «во сколько обойдётся месяц» превращается в заявку:
// @Description у менеджера в проекте загорается плашка, в общий чат уходит
// @Description сообщение. Это ещё не заказ — цену и состав финализирует менеджер.
// @Description Повторный запрос до разбора уточняет ту же заявку, а не заводит вторую.
// @Tags     client-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body monthRequestReq true "ползунки и показанный потолок"
// @Success  201 {object} monthRequestResp
// @Failure  400 {object} errorResponse "bad_id, bad_json, invalid_input"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не ваш"
// @Router   /me/projects/{id}/month-request [post]
func (h *Handler) ClientAskMonth(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	var req monthRequestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	in := AskMonthInput{
		ProjectID: projectID, ClientID: uid,
		Creators: req.Creators, Videos: req.Videos, Ceiling: req.Ceiling,
	}
	if req.Month != "" {
		m, err := time.Parse("2006-01", req.Month)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", "Месяц в формате ГГГГ-ММ.")
			return
		}
		in.Month = m
	}
	out, err := h.svc.AskMonth(r.Context(), in, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, monthRequestResp{Request: &out})
}

// ClientMonthRequest godoc
// @Summary  Открытая заявка на месяц (заказчик)
// @Description Нужна экрану, чтобы вместо кнопки показать «заявка у менеджера»:
// @Description иначе человек жмёт её второй раз, не понимая, ушло ли первое.
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} monthRequestResp "request = null, если заявки нет"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не ваш"
// @Router   /me/projects/{id}/month-request [get]
func (h *Handler) ClientMonthRequest(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	out, err := h.svc.OpenMonthRequest(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, monthRequestResp{Request: out})
}

// ManagerMonthRequest godoc
// @Summary  Заявка заказчика на следующий месяц (менеджер)
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} monthRequestResp "request = null, если заявки нет"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  403 {object} errorResponse "forbidden — проект чужой"
// @Router   /manager/projects/{id}/month-request [get]
func (h *Handler) ManagerMonthRequest(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	out, err := h.svc.OpenMonthRequest(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, monthRequestResp{Request: out})
}

// ManagerHandleMonthRequest godoc
// @Summary  Отметить заявку разобранной (менеджер)
// @Description Плашка гаснет, строка остаётся историей разговора. Идемпотентно.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  204 "заявка разобрана"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  403 {object} errorResponse "forbidden — проект чужой"
// @Router   /manager/projects/{id}/month-request/handled [post]
func (h *Handler) ManagerHandleMonthRequest(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	if err := h.svc.HandleMonthRequest(r.Context(), projectID, uid, time.Now()); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
