package publications

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Доступы к аккаунтам бренда.
//
// Заполняет менеджер, читает заказчик: это его аккаунты, и без них он не
// может ни проверить выкладку, ни забрать доступы обратно. Пароль в
// списке не ездит — он берётся отдельной ручкой, по явному запросу, и
// этот запрос видно в логах.

type accountsResp struct {
	Items []Account `json:"items"`
	// SecretsEnabled — можно ли заводить пароли. Выключено, когда в
	// окружении нет ключа шифрования: логины и ссылки при этом работают,
	// и интерфейсу надо это показать, а не молча прятать поле.
	SecretsEnabled bool `json:"secrets_enabled"`
}

type accountReq struct {
	// CreatorUserID — чей это аккаунт. Пусто — брендовый доступ без
	// владельца (почта, рекламный кабинет).
	CreatorUserID *uuid.UUID `json:"creator_user_id"`
	Platform      string     `json:"platform"`
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	Login         string     `json:"login"`
	// Password: поле не прислали — пароль не трогаем; прислали пустым —
	// стираем.
	Password *string `json:"password"`
	Note     string  `json:"note"`
}

type secretResp struct {
	Password string `json:"password"`
}

func (req accountReq) toInput() AccountInput {
	return AccountInput{
		CreatorUserID: req.CreatorUserID,
		Platform:      req.Platform,
		Title:         req.Title,
		URL:           req.URL,
		Login:         req.Login,
		Password:      req.Password,
		Note:          req.Note,
	}
}

func writeAccountErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrSecretsDisabled) {
		httpx.WriteErrMsg(w, http.StatusNotImplemented, "secrets_disabled",
			"Хранение паролей выключено: в окружении сервиса нет ключа шифрования.")
		return
	}
	if errors.Is(err, ErrAccountExists) {
		httpx.WriteErrMsg(w, http.StatusConflict, "account_exists",
			"У этого креатора уже заведён аккаунт этой площадки — поправьте существующий.")
		return
	}
	if errors.Is(err, ErrCreatorNotInProject) {
		httpx.WriteErrMsg(w, http.StatusConflict, "creator_not_in_project",
			"Этого креатора нет в действующем составе проекта.")
		return
	}
	if errors.Is(err, ErrTooManyAccounts) {
		httpx.WriteErrMsg(w, http.StatusConflict, "too_many_accounts",
			"В проекте слишком много доступов.")
		return
	}
	writeErr(w, err)
}

// ManagerAccounts godoc
// @Summary  Доступы к аккаунтам бренда (менеджер)
// @Description Логины, ссылки и заметки. Пароли в списке не отдаются — только признак has_password.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} accountsResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Router   /manager/projects/{id}/accounts [get]
func (h *Handler) ManagerAccounts(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	items, err := h.svc.Accounts(r.Context(), projectID)
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountsResp{Items: items, SecretsEnabled: h.svc.SecretsEnabled()})
}

// ManagerAddAccount godoc
// @Summary  Завести доступ к аккаунту бренда (менеджер)
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body accountReq true "доступ"
// @Success  201 {object} Account
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Failure      409  {object}  errorResponse  "too_many_accounts"
// @Failure      501  {object}  errorResponse  "secrets_disabled — пароль прислали, а ключа шифрования нет"
// @Router   /manager/projects/{id}/accounts [post]
func (h *Handler) ManagerAddAccount(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req accountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.ManagerAddAccount(r.Context(), projectID, uid, req.toInput())
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, got)
}

// ManagerUpdateAccount godoc
// @Summary  Править доступ (менеджер)
// @Description Пароль не прислали — остаётся прежний: чтобы поправить логин, знать пароль не нужно. Прислали пустым — пароль стирается.
// @Tags     manager-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Param    body body accountReq true "доступ"
// @Success  200 {object} Account
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш или доступа нет"
// @Failure      501  {object}  errorResponse  "secrets_disabled"
// @Router   /manager/projects/{id}/accounts/{account_id} [put]
func (h *Handler) ManagerUpdateAccount(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	accountID, err := uuid.Parse(chi.URLParam(r, "account_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id доступа.")
		return
	}
	var req accountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.ManagerUpdateAccount(r.Context(), projectID, accountID, req.toInput())
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// ManagerRemoveAccount godoc
// @Summary  Удалить доступ (менеджер)
// @Tags     manager-publications
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Success  204
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found"
// @Router   /manager/projects/{id}/accounts/{account_id} [delete]
func (h *Handler) ManagerRemoveAccount(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	accountID, err := uuid.Parse(chi.URLParam(r, "account_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id доступа.")
		return
	}
	if err := h.svc.RemoveAccount(r.Context(), projectID, accountID); err != nil {
		writeAccountErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ManagerAccountSecret godoc
// @Summary  Показать пароль доступа (менеджер)
// @Description Отдельная ручка: пароль не ездит в списке проекта, его запрашивают явно — и запрос видно в логах.
// @Tags     manager-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Success  200 {object} secretResp
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — доступа нет или пароль не заведён"
// @Failure      501  {object}  errorResponse  "secrets_disabled"
// @Router   /manager/projects/{id}/accounts/{account_id}/secret [get]
func (h *Handler) ManagerAccountSecret(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	h.revealSecret(w, r, projectID, uid, "manager")
}

// ClientAccounts godoc
// @Summary  Доступы к аккаунтам бренда (заказчик)
// @Description Это аккаунты заказчика: он вправе видеть, где и под каким логином выходят его ролики. Пароли — отдельной ручкой.
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} accountsResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект не ваш"
// @Router   /me/projects/{id}/accounts [get]
func (h *Handler) ClientAccounts(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	items, err := h.svc.Accounts(r.Context(), projectID)
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountsResp{Items: items, SecretsEnabled: h.svc.SecretsEnabled()})
}

// ClientAccountSecret godoc
// @Summary  Показать пароль доступа (заказчик)
// @Tags     client-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Success  200 {object} secretResp
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — доступа нет или пароль не заведён"
// @Failure      501  {object}  errorResponse  "secrets_disabled"
// @Router   /me/projects/{id}/accounts/{account_id}/secret [get]
func (h *Handler) ClientAccountSecret(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertClient(w, r)
	if !ok {
		return
	}
	h.revealSecret(w, r, projectID, uid, "client")
}

// revealSecret — общая часть показа пароля. Кто и какой доступ открыл,
// пишем в лог: пароли чужих аккаунтов — тот случай, когда «кто смотрел»
// важнее, чем «сколько раз».
func (h *Handler) revealSecret(w http.ResponseWriter, r *http.Request,
	projectID, uid uuid.UUID, role string) {

	accountID, err := uuid.Parse(chi.URLParam(r, "account_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id доступа.")
		return
	}
	pass, err := h.svc.RevealAccountPassword(r.Context(), projectID, accountID)
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	slog.InfoContext(r.Context(), "project account secret revealed",
		"project_id", projectID, "account_id", accountID, "user_id", uid, "role", role)
	httpx.WriteJSON(w, http.StatusOK, secretResp{Password: pass})
}

// ---- «мои аккаунты» у креатора ----

// CreatorAccounts godoc
// @Summary  Мои аккаунты в проекте (креатор)
// @Description Аккаунты ЭТОГО проекта, а не личная страница из профиля: под
// @Description проект креатор заводит отдельные, и ведёт их сам. Чужие и
// @Description брендовые доступы сюда не попадают — в них пароли заказчика.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} accountsResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — вы не в составе проекта"
// @Router   /me/creator/projects/{id}/accounts [get]
func (h *Handler) CreatorAccounts(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertCreator(w, r)
	if !ok {
		return
	}
	items, err := h.svc.CreatorAccounts(r.Context(), projectID, uid)
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accountsResp{Items: items, SecretsEnabled: h.svc.SecretsEnabled()})
}

// CreatorAddAccount godoc
// @Summary  Завести свой аккаунт в проекте (креатор)
// @Description Владелец проставляется сам — завести строку от чужого имени нельзя.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    body body accountReq true "аккаунт"
// @Success  201 {object} Account
// @Failure  400 {object} errorResponse "bad_json; bad_id; invalid_input"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — вы не в составе проекта"
// @Failure  409 {object} errorResponse "too_many_accounts"
// @Failure  501 {object} errorResponse "secrets_disabled — пароль прислали, а ключа шифрования нет"
// @Router   /me/creator/projects/{id}/accounts [post]
func (h *Handler) CreatorAddAccount(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertCreator(w, r)
	if !ok {
		return
	}
	var req accountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.CreatorAddAccount(r.Context(), projectID, uid, req.toInput())
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, got)
}

// CreatorUpdateAccount godoc
// @Summary  Править свой аккаунт (креатор)
// @Description Пароль не прислали — остаётся прежний; прислали пустым — стирается.
// @Tags     creator-publications
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Param    body body accountReq true "аккаунт"
// @Success  200 {object} Account
// @Failure  400 {object} errorResponse "bad_json; bad_id; invalid_input"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — аккаунт не ваш или его нет"
// @Failure  501 {object} errorResponse "secrets_disabled"
// @Router   /me/creator/projects/{id}/accounts/{account_id} [put]
func (h *Handler) CreatorUpdateAccount(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertCreator(w, r)
	if !ok {
		return
	}
	accountID, err := uuid.Parse(chi.URLParam(r, "account_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id доступа.")
		return
	}
	var req accountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	got, err := h.svc.CreatorUpdateAccount(r.Context(), projectID, accountID, uid, req.toInput())
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, got)
}

// CreatorRemoveAccount godoc
// @Summary  Снять свой аккаунт с проекта (креатор)
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Success  204 "удалён"
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — аккаунт не ваш или его нет"
// @Router   /me/creator/projects/{id}/accounts/{account_id} [delete]
func (h *Handler) CreatorRemoveAccount(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertCreator(w, r)
	if !ok {
		return
	}
	accountID, err := uuid.Parse(chi.URLParam(r, "account_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id доступа.")
		return
	}
	if err := h.svc.CreatorRemoveAccount(r.Context(), projectID, accountID, uid); err != nil {
		writeAccountErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreatorAccountSecret godoc
// @Summary  Показать пароль своего аккаунта (креатор)
// @Description Свой пароль забывают так же, как все — он его и вписывал.
// @Tags     creator-publications
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Param    account_id path string true "account id"
// @Success  200 {object} secretResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — аккаунт не ваш, его нет или пароль не записан"
// @Failure  501 {object} errorResponse "secrets_disabled"
// @Router   /me/creator/projects/{id}/accounts/{account_id}/secret [get]
func (h *Handler) CreatorAccountSecret(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.assertCreator(w, r)
	if !ok {
		return
	}
	accountID, err := uuid.Parse(chi.URLParam(r, "account_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id доступа.")
		return
	}
	pass, err := h.svc.CreatorRevealAccountPassword(r.Context(), projectID, accountID, uid)
	if err != nil {
		writeAccountErr(w, err)
		return
	}
	slog.InfoContext(r.Context(), "account secret revealed",
		"project_id", projectID, "account_id", accountID, "actor", uid, "role", "creator")
	httpx.WriteJSON(w, http.StatusOK, secretResp{Password: pass})
}
