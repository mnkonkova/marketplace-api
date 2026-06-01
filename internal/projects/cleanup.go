package projects

import (
	"context"
	"fmt"
	"time"
)

// CleanupOldCompletedProjects — удаляет done-проекты с completed_at старше
// now() - doneRetention И cancelled-проекты с updated_at старше now() -
// cancelledRetention. CASCADE забирает stages/steps/events/comments.
// Возвращает суммарное кол-во удалённых.
//
// Разные retention для done и cancelled: done клиент может всё ещё
// смотреть в кабинете ради записи об отзыве, неделя норм. cancelled
// держим дольше — там может всплыть жалоба, нужны следы.
func (r *Repo) CleanupOldCompletedProjects(ctx context.Context, doneRetention, cancelledRetention time.Duration) (int, error) {
	total := 0
	if doneRetention > 0 {
		cutoff := time.Now().Add(-doneRetention)
		tag, err := r.db.Exec(ctx,
			`DELETE FROM projects
			 WHERE status = 'done' AND completed_at IS NOT NULL AND completed_at < $1`,
			cutoff)
		if err != nil {
			return total, fmt.Errorf("cleanup done projects: %w", err)
		}
		total += int(tag.RowsAffected())
	}
	if cancelledRetention > 0 {
		cutoff := time.Now().Add(-cancelledRetention)
		tag, err := r.db.Exec(ctx,
			`DELETE FROM projects
			 WHERE status = 'cancelled' AND updated_at < $1`,
			cutoff)
		if err != nil {
			return total, fmt.Errorf("cleanup cancelled projects: %w", err)
		}
		total += int(tag.RowsAffected())
	}
	return total, nil
}

// RunOldProjectsCleanup — sugar обёртка для worker-тикера.
func (s *Service) RunOldProjectsCleanup(ctx context.Context, doneRetention, cancelledRetention time.Duration) (int, error) {
	return s.repo.CleanupOldCompletedProjects(ctx, doneRetention, cancelledRetention)
}
