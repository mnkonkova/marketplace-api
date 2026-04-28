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

func (i *Indexer) Reconcile(ctx context.Context, userID uuid.UUID) error {
	doc, err := i.repo.LoadDoc(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return i.es.DeleteDoc(ctx, i.index, userID.String())
	}
	if err != nil {
		return fmt.Errorf("load doc: %w", err)
	}
	if !doc.IsPublished {
		return i.es.DeleteDoc(ctx, i.index, userID.String())
	}
	return i.es.IndexDoc(ctx, i.index, userID.String(), doc)
}

func (i *Indexer) Delete(ctx context.Context, userID uuid.UUID) error {
	return i.es.DeleteDoc(ctx, i.index, userID.String())
}
