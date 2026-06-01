package projects

import (
	"context"
	"fmt"
	"time"
)

// CleanupOldCompletedProjects — удаляет проекты со status='done' у которых
// completed_at старше cutoff = now() - retention. CASCADE забирает
// project_stages/steps/events/comments. Возвращает кол-во удалённых
// (для логов worker'а).
//
// cancelled-проекты не трогаем — там могут быть важные следы (дисп. почему
// клиент свалил, какая стадия зависла). Чистка должна быть про «успешно
// закрылся отзывом» — тогда снимок не нужен.
func (r *Repo) CleanupOldCompletedProjects(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention)
	tag, err := r.db.Exec(ctx,
		`DELETE FROM projects
		 WHERE status = 'done' AND completed_at IS NOT NULL AND completed_at < $1`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("cleanup old projects: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RunOldProjectsCleanup — sugar обёртка для worker-тикера.
func (s *Service) RunOldProjectsCleanup(ctx context.Context, retention time.Duration) (int, error) {
	return s.repo.CleanupOldCompletedProjects(ctx, retention)
}
