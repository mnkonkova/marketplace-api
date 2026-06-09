package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
	"marketpclce/internal/profiles"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func parseID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id в URL.")
		return uuid.Nil, false
	}
	return id, true
}

func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		// data-sec D12: см. writeManagerErr в projects/handlers_manager.go.
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrModerationReasonRequired):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "reason_required",
			"Укажите причину отклонения (минимум 3 символа).")
	case errors.Is(err, ErrNotFound), errors.Is(err, profiles.ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Пользователь не найден.")
	case errors.Is(err, profiles.ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "conflict",
			"Специалист отредактировал профиль с момента открытия — перезагрузите карточку и проверьте изменения.")
	case errors.Is(err, ErrNotManager):
		httpx.WriteErrMsg(w, http.StatusConflict, "not_manager",
			"Этот пользователь не имеет роли менеджера.")
	case errors.Is(err, ErrAlreadyExists):
		httpx.WriteErrMsg(w, http.StatusConflict, "already_exists",
			"Пользователь с таким email уже существует.")
	case errors.Is(err, ErrInviteInvalid):
		httpx.WriteErrMsg(w, http.StatusGone, "invite_invalid",
			"Ссылка инвайта недействительна или просрочена.")
	default:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// AdminListManagers godoc
// @Summary  Список менеджеров (с фильтром is_approved)
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    is_approved query bool false "true|false"
// @Success  200 {object} managersListResp
// @Router   /admin/managers [get]
func (h *Handler) AdminListManagers(w http.ResponseWriter, r *http.Request) {
	var approved *bool
	if v := strings.TrimSpace(r.URL.Query().Get("is_approved")); v != "" {
		b := v == "true" || v == "1"
		approved = &b
	}
	items, err := h.svc.ListManagers(r.Context(), approved)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managersListResp{Items: items})
}

// AdminSearchUsers godoc
// @Summary  Поиск юзеров (для лукапов в CRM)
// @Description ILIKE по email/phone/display_name. Только активные,
// @Description фильтр kind=client|specialist. q короче 2 символов — пусто.
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    q    query string true  "часть email/phone/имени, мин 2 символа"
// @Param    kind query string true  "client | specialist"
// @Success  200 {object} userSearchResp
// @Router   /admin/users/search [get]
func (h *Handler) AdminSearchUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "all" && kind != "client" && kind != "specialist" {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_kind",
			"kind должен быть client, specialist или all.")
		return
	}
	items, err := h.svc.SearchUsers(r.Context(), q, kind)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, userSearchResp{Items: items})
}

type promoteManagerReq struct {
	UserID     string `json:"user_id"`
	SendInvite bool   `json:"send_invite,omitempty"`
}

// AdminPromoteToManager godoc
// @Summary  Сделать существующего юзера менеджером (+ опционально invite)
// @Description Ищется юзер через /admin/users/search, далее этот endpoint
// @Description выставляет is_manager=TRUE и is_approved=TRUE. send_invite=true
// @Description дополнительно генерит magic-link для входа.
// @Tags     admin-users
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body promoteManagerReq true "user_id + send_invite"
// @Success  200 {object} InviteGenerateResult "{} если send_invite=false"
// @Router   /admin/managers/promote [post]
func (h *Handler) AdminPromoteToManager(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return
	}
	var in promoteManagerReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	userID, err := uuid.Parse(strings.TrimSpace(in.UserID))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_user_id",
			"user_id обязателен и должен быть UUID.")
		return
	}
	res, err := h.svc.PromoteToManager(r.Context(), userID, actor, in.SendInvite)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// AdminApproveManager godoc
// @Summary  Аппрувить менеджера
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  204
// @Router   /admin/managers/{id}/approve [post]
func (h *Handler) AdminApproveManager(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.ApproveManager(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminRevokeManager godoc
// @Summary  Снять аппрув с менеджера
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  204
// @Router   /admin/managers/{id}/revoke [post]
func (h *Handler) AdminRevokeManager(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.RevokeManager(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createClientReq struct {
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
	GenerateInvite  bool   `json:"generate_invite"`
}

// AdminCreateClient godoc
// @Summary  Создать клиента вручную (опц. сгенерить инвайт)
// @Tags     admin-users
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body createClientReq true "client data"
// @Success  201 {object} CreateClientResult
// @Router   /admin/users [post]
func (h *Handler) AdminCreateClient(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	var in createClientReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	res, err := h.svc.CreateClient(r.Context(), CreateClientInput{
		Email: in.Email, DisplayName: in.DisplayName,
	}, in.GenerateInvite, actorID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

// ManagerGenerateInvite — то же что AdminGenerateInvite, но для роли manager.
// Менеджеру нужно перевыпускать magic-link клиентам своих проектов.
// Доступ контролируется RequireRoles(RoleManager, RoleAdmin) в router.
func (h *Handler) ManagerGenerateInvite(w http.ResponseWriter, r *http.Request) {
	h.AdminGenerateInvite(w, r)
}

// AdminGenerateInvite godoc
// @Summary  Сгенерировать инвайт для существующего юзера
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  200 {object} InviteGenerateResult
// @Router   /admin/users/{id}/generate_invite [post]
func (h *Handler) AdminGenerateInvite(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	res, err := h.svc.GenerateInvite(r.Context(), id, actorID)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type redeemResp struct {
	UserID string         `json:"user_id"`
	Tokens auth.TokenPair `json:"tokens"`
}

// RedeemInvite godoc
// @Summary  Обменять magic-link на JWT (публичный)
// @Tags     auth
// @Produce  json
// @Param    token path string true "raw invite token"
// @Success  200 {object} redeemResp
// @Failure  410 {object} errorResponse "invite_invalid"
// @Router   /auth/redeem_invite/{token} [post]
func (h *Handler) RedeemInvite(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	pair, userID, err := h.svc.RedeemInvite(r.Context(), token)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, redeemResp{
		UserID: userID.String(),
		Tokens: pair,
	})
}

// ─── Модерация специалистов ──────────────────────────────────────────────
// См. docs/SPECIALIST_MODERATION.md

type moderationListResp struct {
	Items []profiles.ModerationQueueItem `json:"items"`
	Total int                            `json:"total"`
}

// AdminListPendingSpecialists godoc
// @Summary  Очередь модерации специалистов
// @Tags     admin-moderation
// @Produce  json
// @Security BearerAuth
// @Param    status query string false "pending_review (default) | approved | rejected | all"
// @Param    limit  query int    false "1-100, default 20"
// @Param    offset query int    false "default 0"
// @Success  200 {object} moderationListResp
// @Router   /admin/moderation/specialists [get]
func (h *Handler) AdminListPendingSpecialists(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, total, err := h.svc.ListPendingSpecialists(r.Context(), status, limit, offset)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, moderationListResp{Items: items, Total: total})
}

type moderationCountResp struct {
	PendingCount int `json:"pending_count"`
}

// AdminPendingModerationCount godoc
// @Summary  Счётчик заявок на модерации (для бейджа в навигации)
// @Tags     admin-moderation
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} moderationCountResp
// @Router   /admin/moderation/specialists/count [get]
func (h *Handler) AdminPendingModerationCount(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.CountPendingSpecialists(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, moderationCountResp{PendingCount: n})
}

// AdminGetSpecialistForModeration godoc
// @Summary  Полная карточка спеца для admin'ского просмотра
// @Description В отличие от /specialists/{id} (только approved+published),
// @Description эта ручка отдаёт и pending, и rejected — чтобы админ мог
// @Description принимать решение.
// @Tags     admin-moderation
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  200 {object} profiles.PublicProfile
// @Router   /admin/moderation/specialists/{id} [get]
func (h *Handler) AdminGetSpecialistForModeration(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	p, err := h.svc.GetSpecialistForModeration(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

type approveReq struct {
	// ExpectedUpdatedAt — optimistic-lock версия профиля, которую видел
	// админ при открытии карточки. Если null — без проверки (для тестов
	// или legacy-CLI). См. docs/SPECIALIST_MODERATION.md §5.C.
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at,omitempty"`
}

// AdminApproveSpecialist godoc
// @Summary  Одобрить публикацию специалиста (попадает в каталог)
// @Tags     admin-moderation
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Param    body body approveReq false "expected_updated_at для optimistic lock"
// @Success  204
// @Failure  409 {object} errorResponse "conflict — спец отредактировал, перезагрузить"
// @Router   /admin/moderation/specialists/{id}/approve [post]
func (h *Handler) AdminApproveSpecialist(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return
	}
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in approveReq
	// Тело опционально (legacy-клиенты без updated_at).
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
			return
		}
	}
	if err := h.svc.ApproveSpecialist(r.Context(), id, actor, in.ExpectedUpdatedAt); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type rejectReq struct {
	Reason            string     `json:"reason"`
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at,omitempty"`
}

// AdminRejectSpecialist godoc
// @Summary  Отклонить публикацию специалиста с указанием причины
// @Tags     admin-moderation
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "user id"
// @Param    body body rejectReq true "причина (3-500 симв) + expected_updated_at"
// @Success  204
// @Failure  409 {object} errorResponse "conflict — спец отредактировал, перезагрузить"
// @Router   /admin/moderation/specialists/{id}/reject [post]
func (h *Handler) AdminRejectSpecialist(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return
	}
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in rejectReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	if err := h.svc.RejectSpecialist(r.Context(), id, actor, in.Reason, in.ExpectedUpdatedAt); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// типы для swaggo
type managersListResp struct {
	Items []ManagerInfo `json:"items"`
}

type userSearchResp struct {
	Items []UserSearchResult `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
