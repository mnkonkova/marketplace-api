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
	Platform string `json:"platform"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Login    string `json:"login"`
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
		Platform: req.Platform,
		Title:    req.Title,
		URL:      req.URL,
		Login:    req.Login,
		Password: req.Password,
		Note:     req.Note,
	}
}

func writeAccountErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrSecretsDisabled) {
		httpx.WriteErrMsg(w, http.StatusNotImplemented, "secrets_disabled",
			"Хранение паролей выключено: в окружении сервиса нет ключа шифрования.")
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
