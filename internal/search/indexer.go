package search

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"marketpclce/internal/platform/es"
)

type Indexer struct {
	repo  *Repo
	es    *es.Client
	index string
}

func NewIndexer(repo *Repo, esClient *es.Client, index string) *Indexer {
	return &Indexer{repo: repo, es: esClient, index: index}
}

// Reconcile — синхронизирует один документ спеца в ES. version — внешний
// монотонный номер (микросекунды updated_at) для optimistic concurrency
// control. Если >0, OS отклоняет write/delete с 409 если у него уже есть
// более свежая версия — ES-клиент это проглатывает как nil (см.
// IndexDocVer/DeleteDocVer). version=0 → fallback без проверки версии.
func (i *Indexer) Reconcile(ctx context.Context, userID uuid.UUID, version int64) error {
	doc, err := i.repo.LoadDoc(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return i.es.DeleteDocVer(ctx, i.index, userID.String(), version)
	}
	if err != nil {
		return fmt.Errorf("load doc: %w", err)
	}
	// В каталог попадают только опубликованные И одобренные админом.
	// Pending/rejected — снимаются из ES; см. docs/SPECIALIST_MODERATION.md.
	if !doc.IsPublished || doc.ModerationStatus != "approved" {
		return i.es.DeleteDocVer(ctx, i.index, userID.String(), version)
	}
	return i.es.IndexDocVer(ctx, i.index, userID.String(), doc, version)
}

func (i *Indexer) Delete(ctx context.Context, userID uuid.UUID, version int64) error {
	return i.es.DeleteDocVer(ctx, i.index, userID.String(), version)
}
