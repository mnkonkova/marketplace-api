package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const commentMaxLen = 5000

// ListComments — общий доступ к комментариям. Право видеть проверяется в
// handler-слое (manager/admin: GetByIDForManager; client: GetByIDForClient).
func (s *Service) ListComments(ctx context.Context, projectID uuid.UUID) ([]Comment, error) {
	return s.repo.ListComments(ctx, projectID)
}

// CreateComment — пока только manager/admin (роль проверяется на роутере).
// Клиент в MVP комментарии не пишет (по брифу § 5.1 — только GET).
func (s *Service) CreateComment(ctx context.Context, projectID, authorID uuid.UUID, body string) (Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Comment{}, ErrCommentEmpty
	}
	if len(body) > commentMaxLen {
		return Comment{}, fmt.Errorf("%w: body too long (max %d)", ErrInvalidInput, commentMaxLen)
	}
	return s.repo.CreateComment(ctx, projectID, authorID, body, "plain")
}

// ListEvents — лента активности. Авторизация — в handler.
func (s *Service) ListEvents(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]Event, error) {
	events, err := s.repo.ListEvents(ctx, projectID, limit, offset)
	if err != nil {
		return nil, err
	}
	return events, nil
}

// ensureNotFoundCompat — нужен только чтобы errors.Is из handler-слоя
// мапился корректно. Конкретно ErrNotFound уже определён в repo.go.
var _ = errors.Is
