package publications

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// ---- шаблоны документов: админ ----

type documentTemplatesResp struct {
	Items []DocumentTemplate `json:"items"`
}

type createDocumentTemplateReq struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Audience string `json:"audience"`
	URL      string `json:"url"`
	Note     string `json:"note"`
}

type publishTemplateVersionReq struct {
	URL  string `json:"url"`
	Note string `json:"note"`
}

// AdminListDocumentTemplates godoc
// @Summary  Шаблоны документов (админ)
// @Description Библиотека шаблонов с историей версий. archived=1 — вместе с
// @Description архивом: он не удаляется, а уходит из выбора у менеджера.
// @Tags     admin-documents
// @Produce  json
// @Security BearerAuth
// @Param    archived query string false "1 — показать и архивные"
// @Success  200 {object} documentTemplatesResp
// @Router   /admin/document_templates [get]
func (h *Handler) AdminListDocumentTemplates(w http.ResponseWriter, r *http.Request) {
	withArchived := r.URL.Query().Get("archived") == "1"
	items, err := h.svc.ListDocumentTemplates(r.Context(), withArchived, true, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, documentTemplatesResp{Items: items})
}

// AdminCreateDocumentTemplate godoc
// @Summary  Завести шаблон документа (админ)
// @Description Шаблон и его первая версия. Документы — только ссылки.
// @Tags     admin-documents
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body createDocumentTemplateReq true "шаблон"
// @Success  201 {object} DocumentTemplate
// @Failure  400 {object} errorResponse "invalid_input"
// @Router   /admin/document_templates [post]
func (h *Handler) AdminCreateDocumentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	var req createDocumentTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	t, err := h.svc.CreateDocumentTemplate(r.Context(), actor,
		req.Kind, req.Title, req.Audience, req.URL, req.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, t)
}

// AdminPublishTemplateVersion godoc
// @Summary  Новая версия шаблона (админ)
// @Description Версии не правятся: выданные документы ссылаются на свою
// @Description версию, и новая их не трогает. Как у прайса.
// @Tags     admin-documents
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string true "template id"
// @Param    body body publishTemplateVersionReq true "версия"
// @Success  201 {object} TemplateVersion
// @Failure  400 {object} errorResponse "invalid_input"
// @Failure  404 {object} errorResponse "not_found"
// @Failure  409 {object} errorResponse "template_archived"
// @Router   /admin/document_templates/{id}/versions [post]
func (h *Handler) AdminPublishTemplateVersion(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id шаблона.")
		return
	}
	var req publishTemplateVersionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	v, err := h.svc.PublishTemplateVersion(r.Context(), actor, id, req.URL, req.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, v)
}

// AdminArchiveDocumentTemplate godoc
// @Summary  Шаблон в архив (админ)
// @Description Не удаление: из выбора у менеджера уходит, история и выданные
// @Description по нему документы остаются. Вернуть — /restore.
// @Tags     admin-documents
// @Security BearerAuth
// @Param    id path string true "template id"
// @Success  204
// @Failure  404 {object} errorResponse "not_found"
// @Router   /admin/document_templates/{id}/archive [post]
func (h *Handler) AdminArchiveDocumentTemplate(w http.ResponseWriter, r *http.Request) {
	h.setTemplateArchived(w, r, true)
}

// AdminRestoreDocumentTemplate godoc
// @Summary  Вернуть шаблон из архива (админ)
// @Tags     admin-documents
// @Security BearerAuth
// @Param    id path string true "template id"
// @Success  204
// @Failure  404 {object} errorResponse "not_found"
// @Router   /admin/document_templates/{id}/restore [post]
func (h *Handler) AdminRestoreDocumentTemplate(w http.ResponseWriter, r *http.Request) {
	h.setTemplateArchived(w, r, false)
}

func (h *Handler) setTemplateArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	actor, _ := auth.UserIDFrom(r.Context())
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id шаблона.")
		return
	}
	if err := h.svc.SetTemplateArchived(r.Context(), actor, id, archived); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- выдача: менеджер ----

type userDocumentsResp struct {
	Items []UserDocument `json:"items"`
}

type deliverDocumentReq struct {
	// Audience — creators или client.
	Audience string `json:"audience"`
	// RecipientIDs — кому. Пусто — всему составу или заказчику проекта.
	RecipientIDs []string `json:"recipient_ids"`
	// TemplateID — выдать действующую версию шаблона. Пусто — своя
	// ссылка: тогда нужны kind, title и url.
	TemplateID string `json:"template_id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Note       string `json:"note"`
}

// ManagerDocumentTemplates godoc
// @Summary  Шаблоны документов для выдачи (менеджер)
// @Description Только действующие, с текущей версией. audience — для кого.
// @Tags     manager-documents
// @Produce  json
// @Security BearerAuth
// @Param    audience query string false "creators | client"
// @Success  200 {object} documentTemplatesResp
// @Router   /manager/document_templates [get]
func (h *Handler) ManagerDocumentTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListDocumentTemplates(r.Context(), false, false,
		strings.TrimSpace(r.URL.Query().Get("audience")))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, documentTemplatesResp{Items: items})
}

// ManagerProjectDocuments godoc
// @Summary  Выданные документы проекта (менеджер)
// @Description Кому что и когда выдали, открыли ли; отозванные тоже — с
// @Description отметкой.
// @Tags     manager-documents
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} userDocumentsResp
// @Failure  404 {object} errorResponse "not_found"
// @Router   /manager/projects/{id}/documents [get]
func (h *Handler) ManagerProjectDocuments(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ProjectDocuments(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, userDocumentsResp{Items: items})
}

// ManagerDeliverDocument godoc
// @Summary  Выдать документ (менеджер)
// @Description Одному человеку, нескольким или всем по стороне проекта.
// @Description Адресат получает сообщение в бот; документ появляется в
// @Description «Моих документах».
// @Tags     manager-documents
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string true "project id"
// @Param    body body deliverDocumentReq true "что и кому"
// @Success  201 {object} userDocumentsResp
// @Failure  400 {object} errorResponse "invalid_input — адресат не в проекте, нет ссылки и т.п."
// @Failure  404 {object} errorResponse "not_found — проекта или шаблона нет"
// @Failure  409 {object} errorResponse "template_archived"
// @Router   /manager/projects/{id}/documents [post]
func (h *Handler) ManagerDeliverDocument(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req deliverDocumentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	in := DeliverInput{
		ProjectID: projectID, Audience: strings.TrimSpace(req.Audience),
		Kind: req.Kind, Title: req.Title, URL: req.URL, Note: req.Note, SentBy: uid,
	}
	for _, s := range req.RecipientIDs {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "recipient_ids — список UUID.")
			return
		}
		in.RecipientIDs = append(in.RecipientIDs, id)
	}
	if s := strings.TrimSpace(req.TemplateID); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "template_id должен быть UUID.")
			return
		}
		in.TemplateID = id
	}
	items, err := h.svc.DeliverDocument(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, userDocumentsResp{Items: items})
}

// ManagerRevokeDocument godoc
// @Summary  Отозвать документ (менеджер)
// @Description У адресата пропадает, в истории выдачи остаётся. Вернуть —
// @Description /unrevoke.
// @Tags     manager-documents
// @Security BearerAuth
// @Param    id     path string true "project id"
// @Param    doc_id path string true "document id"
// @Success  204
// @Failure  404 {object} errorResponse "not_found"
// @Router   /manager/projects/{id}/documents/{doc_id}/revoke [post]
func (h *Handler) ManagerRevokeDocument(w http.ResponseWriter, r *http.Request) {
	h.setDocumentRevoked(w, r, true)
}

// ManagerUnrevokeDocument godoc
// @Summary  Вернуть отозванный документ (менеджер)
// @Tags     manager-documents
// @Security BearerAuth
// @Param    id     path string true "project id"
// @Param    doc_id path string true "document id"
// @Success  204
// @Failure  404 {object} errorResponse "not_found"
// @Router   /manager/projects/{id}/documents/{doc_id}/unrevoke [post]
func (h *Handler) ManagerUnrevokeDocument(w http.ResponseWriter, r *http.Request) {
	h.setDocumentRevoked(w, r, false)
}

func (h *Handler) setDocumentRevoked(w http.ResponseWriter, r *http.Request, revoked bool) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	docID, err := pathUUID(r, "doc_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id документа.")
		return
	}
	if err := h.svc.SetDocumentRevoked(r.Context(), projectID, docID, uid, revoked); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- мои документы: креатор и заказчик ----

type myDocumentsResp struct {
	Items []MyDocument `json:"items"`
}

type openedResp struct {
	OpenedAt string `json:"opened_at"`
}

// MyDocuments godoc
// @Summary  Мои документы
// @Description Выданное лично и договоры из материалов проектов, где человек
// @Description сейчас работает или заказчик. Отозванное не показывается.
// @Description С project_id — только документы этого проекта, договор первым.
// @Description С source=personal — только выданное лично, без договоров из
// @Description материалов проекта.
// @Tags     me-documents
// @Produce  json
// @Security BearerAuth
// @Param    project_id query string false "только этот проект"
// @Param    source     query string false "personal — только выданное лично"
// @Success  200 {object} myDocumentsResp
// @Failure  400 {object} errorResponse "invalid_input — project_id не uuid или неизвестный source"
// @Failure  401 {object} errorResponse "no_user"
// @Router   /me/documents [get]
func (h *Handler) MyDocuments(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	var f MyDocumentsFilter
	q := r.URL.Query()
	if raw := q.Get("project_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: project_id must be a uuid", ErrInvalidInput))
			return
		}
		f.ProjectID = &id
	}
	switch q.Get("source") {
	case "":
	case "personal":
		f.PersonalOnly = true
	default:
		writeErr(w, fmt.Errorf("%w: source must be personal", ErrInvalidInput))
		return
	}
	items, err := h.svc.MyDocuments(r.Context(), uid, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, myDocumentsResp{Items: items})
}

// OpenMyDocument godoc
// @Summary  Отметить документ открытым
// @Description Ставится один раз — менеджер видит, что документ дошёл.
// @Description Только для выданных лично (source=personal).
// @Tags     me-documents
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "document id"
// @Success  200 {object} openedResp
// @Failure  404 {object} errorResponse "not_found — чужой или отозванный"
// @Router   /me/documents/{id}/open [post]
func (h *Handler) OpenMyDocument(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id документа.")
		return
	}
	at, err := h.svc.MarkDocumentOpened(r.Context(), uid, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, openedResp{OpenedAt: at.UTC().Format("2006-01-02T15:04:05Z07:00")})
}
