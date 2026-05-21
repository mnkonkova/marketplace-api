package search

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"marketpclce/internal/platform/es"
)

type FeedIndexer struct {
	repo  *Repo
	es    *es.Client
	index string
}

func NewFeedIndexer(repo *Repo, esClient *es.Client, index string) *FeedIndexer {
	return &FeedIndexer{repo: repo, es: esClient, index: index}
}

// ReconcileVideos — синхронизирует все видео-документы одного спеца в ES.
// Алгоритм: грузим из PG все актуальные видео (с denorm'ными полями спеца),
// удаляем все существующие документы этого user_id (delete_by_query) и
// upsert'ом пишем новые. Это идемпотентно: повторный вызов даёт тот же
// результат. Если спец не публикуется или у него нет видео — просто
// зачищаем индекс.
//
// Альтернатива «обновить только дельту» не стоит свеч: при изменении профиля
// (rating_avg, display_name) нужно переписать все его видео всё равно.
func (i *FeedIndexer) ReconcileVideos(ctx context.Context, userID uuid.UUID) error {
	docs, err := i.repo.LoadFeedVideoDocs(ctx, userID)
	if err != nil {
		return fmt.Errorf("load feed docs: %w", err)
	}

	// Снести всё что есть у этого user_id. Если документов нет — no-op.
	if err := i.deleteByUser(ctx, userID); err != nil {
		return fmt.Errorf("delete previous: %w", err)
	}

	for _, d := range docs {
		if err := i.es.IndexDoc(ctx, i.index, d.VideoID, d); err != nil {
			return fmt.Errorf("index video %s: %w", d.VideoID, err)
		}
	}
	return nil
}

// DeleteByUser — снимает все видео-доки спеца. Вызывается при специальном
// удалении профиля; при unpublish ReconcileVideos сам зачистит (LoadFeedVideoDocs
// фильтрует по is_published=TRUE).
func (i *FeedIndexer) DeleteByUser(ctx context.Context, userID uuid.UUID) error {
	return i.deleteByUser(ctx, userID)
}

func (i *FeedIndexer) deleteByUser(ctx context.Context, userID uuid.UUID) error {
	return i.es.DeleteByQuery(ctx, i.index, map[string]any{
		"query": map[string]any{
			"term": map[string]any{"user_id": userID.String()},
		},
	})
}

// IsEmpty — true если в индексе нет документов. Используется bootstrap'ом
// воркера: при первом запуске после деплоя Stage 2 индекс пуст, надо прогнать
// всех опубликованных спецов.
func (i *FeedIndexer) IsEmpty(ctx context.Context) (bool, error) {
	n, err := i.es.CountDocs(ctx, i.index)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// Bootstrap — прогоняет ReconcileVideos для всех опубликованных спецов.
// Идемпотентно и безопасно вызывать многократно — лишний раз перепишет
// доки. Используется при первом старте воркера на новом индексе.
func (i *FeedIndexer) Bootstrap(ctx context.Context) (int, error) {
	ids, err := i.repo.LoadPublishedSpecialistIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("load specialists: %w", err)
	}
	for _, id := range ids {
		if err := i.ReconcileVideos(ctx, id); err != nil {
			return 0, fmt.Errorf("reconcile %s: %w", id, err)
		}
	}
	return len(ids), nil
}
