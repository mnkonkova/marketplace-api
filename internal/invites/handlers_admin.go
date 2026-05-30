package invites

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// ClientCreator — узкий контракт для создания нового user-клиента
// в БД. Реализуется адаптером в main.go поверх auth.Service (или прямого
// SQL). Возвращает userID существующего или нового user'а; alreadyExisted
// сообщает, нужно ли отрисовать «уже есть».
type ClientCreator interface {
	CreateClient(ctx context.Context, email, displayName string) (userID uuid.UUID, alreadyExisted bool, err error)
}

type Handler struct {
	svc     *Service
	creator ClientCreator
}

func NewHandler(svc *Service, creator ClientCreator) *Handler {
	return &Handler{svc: svc, creator: creator}
}

// AdminCreateUserInput — body POST /admin/users.
type AdminCreateUserInput struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	SendInvite bool   `json:"send_invite"`
}

// AdminCreateUserResponse — что отдаём админу. Token присутствует только
// если send_invite=true (показать менеджеру для копи-паста, на случай
// если письмо не дошло). Если уже существовал — already_existed=true.
type AdminCreateUserResponse struct {
	UserID          uuid.UUID `json:"user_id"`
	AlreadyExisted  bool      `json:"already_existed"`
	InviteToken     string    `json:"invite_token,omitempty"`
	InviteExpiresAt string    `json:"invite_expires_at,omitempty"`
}

// AdminCreateUser godoc
// @Summary      Создать клиента вручную (admin)
// @Description  Используется Directus Flow «Create manual project». Если send_invite=true — сразу выпускается magic-link и публикуется outbox-событие client_invite.generated (n8n шлёт письмо).
// @Tags         admin-users
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body  body      AdminCreateUserInput  true  "email + name + send_invite"
// @Success      201   {object}  AdminCreateUserResponse
// @Failure      400   {object}  errorResponse
// @Router       /admin/users [post]
func (h *Handler) AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	var in AdminCreateUserInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	if in.Email == "" {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", "email обязателен")
		return
	}

	userID, existed, err := h.creator.CreateClient(r.Context(), in.Email, in.Name)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось создать клиента")
		return
	}

	resp := AdminCreateUserResponse{
		UserID:         userID,
		AlreadyExisted: existed,
	}
	if in.SendInvite {
		gen, err := h.svc.Generate(r.Context(), GenerateInput{
			UserID:    userID,
			CreatedBy: actor,
		})
		if err != nil {
			// Юзер создан, но invite упал. Возвращаем 201 с предупреждением.
			httpx.WriteJSON(w, http.StatusCreated, resp)
			return
		}
		resp.InviteToken = gen.RawToken
		resp.InviteExpiresAt = gen.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
	}

	httpx.WriteJSON(w, http.StatusCreated, resp)
}

// AdminGenerateInvite godoc
// @Summary      Сгенерировать magic-link для существующего user'а
// @Tags         admin-users
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "User ID"
// @Success      201  {object}  GenerateResult
// @Failure      400  {object}  errorResponse
// @Router       /admin/users/{id}/generate_invite [post]
func (h *Handler) AdminGenerateInvite(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный user ID")
		return
	}
	gen, err := h.svc.Generate(r.Context(), GenerateInput{
		UserID:    id,
		CreatedBy: actor,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось выпустить инвайт")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, gen)
}

type errorResponse struct {
	Error string `json:"error"`
}
