package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	base string
	http *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

type ErrStatus struct {
	Status int
	Body   string
}

func (e *ErrStatus) Error() string { return fmt.Sprintf("opensearch %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensearch %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &ErrStatus{Status: resp.StatusCode, Body: string(respBody)}
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) IndexExists(ctx context.Context, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.base+"/"+name, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return false, &ErrStatus{Status: resp.StatusCode, Body: string(body)}
	}
}

func (c *Client) CreateIndex(ctx context.Context, name string, mapping any) error {
	return c.do(ctx, http.MethodPut, "/"+name, mapping, nil)
}

func (c *Client) EnsureIndex(ctx context.Context, name string, mapping any) error {
	exists, err := c.IndexExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return c.CreateIndex(ctx, name, mapping)
}

func (c *Client) IndexDoc(ctx context.Context, index, id string, doc any) error {
	return c.do(ctx, http.MethodPut, "/"+index+"/_doc/"+id+"?refresh=false", doc, nil)
}

// BulkDoc — пара (id, doc) для пакетной индексации.
type BulkDoc struct {
	ID  string
	Doc any
}

// BulkIndex — один POST /_bulk вместо N PUT'ов. Используется feed_indexer.
// ReconcileVideos для спеца с N видео (P7): раньше 30 round-trip'ов на
// внутри одной outbox-tx (см. P3), теперь один request.
//
// Тело — NDJSON: попеременно action ("index"+_id) и сам doc, разделены \n.
// Ошибки парсятся: при errors=true собираем перечисление "id: reason" и
// возвращаем как ErrStatus, иначе nil. Частичный успех (например, 9 из 10
// прошло) трактуем как ошибку — feed_indexer считает spec'a несинхронным.
func (c *Client) BulkIndex(ctx context.Context, index string, docs []BulkDoc) error {
	if len(docs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, d := range docs {
		action := map[string]any{
			"index": map[string]any{"_index": index, "_id": d.ID},
		}
		actionJSON, err := json.Marshal(action)
		if err != nil {
			return fmt.Errorf("marshal bulk action: %w", err)
		}
		docJSON, err := json.Marshal(d.Doc)
		if err != nil {
			return fmt.Errorf("marshal bulk doc %s: %w", d.ID, err)
		}
		buf.Write(actionJSON)
		buf.WriteByte('\n')
		buf.Write(docJSON)
		buf.WriteByte('\n')
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/_bulk?refresh=false", &buf)
	if err != nil {
		return err
	}
	// /_bulk требует именно application/x-ndjson, иначе OS не разделит документы.
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensearch bulk: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &ErrStatus{Status: resp.StatusCode, Body: string(respBody)}
	}
	var parsed struct {
		Errors bool `json:"errors"`
		Items  []struct {
			Index struct {
				ID     string `json:"_id"`
				Status int    `json:"status"`
				Error  *struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"error,omitempty"`
			} `json:"index"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("decode bulk response: %w", err)
	}
	if parsed.Errors {
		var firstErr string
		for _, it := range parsed.Items {
			if it.Index.Error != nil {
				firstErr = fmt.Sprintf("%s: %s", it.Index.ID, it.Index.Error.Reason)
				break
			}
		}
		return fmt.Errorf("bulk partial failure: %s", firstErr)
	}
	return nil
}

func (c *Client) DeleteDoc(ctx context.Context, index, id string) error {
	err := c.do(ctx, http.MethodDelete, "/"+index+"/_doc/"+id, nil, nil)
	var es *ErrStatus
	if errors.As(err, &es) && es.Status == http.StatusNotFound {
		return nil
	}
	return err
}

type SearchHit struct {
	ID     string          `json:"_id"`
	Score  float64         `json:"_score"`
	Source json.RawMessage `json:"_source"`
	// Sort — массив значений по которым ES отсортировал хит. Используется
	// как курсор для search_after-пагинации (передаём ровно эти значения
	// в следующий запрос). Пустой если в запросе не указан sort.
	Sort []any `json:"sort,omitempty"`
}

type SearchResponse struct {
	Took int `json:"took"`
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []SearchHit `json:"hits"`
	} `json:"hits"`
	Aggregations json.RawMessage `json:"aggregations,omitempty"`
}

func (c *Client) Search(ctx context.Context, index string, query any) (*SearchResponse, error) {
	var resp SearchResponse
	if err := c.do(ctx, http.MethodPost, "/"+index+"/_search", query, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteByQuery — массовое удаление по запросу. Используется feed-индексером
// чтобы при reconcile одного спеца снести все его видео-доки одной командой.
func (c *Client) DeleteByQuery(ctx context.Context, index string, query any) error {
	return c.do(ctx, http.MethodPost, "/"+index+"/_delete_by_query?refresh=false", query, nil)
}

// CountDocs — сколько документов в индексе. Используется при bootstrap'е
// feed_videos: если 0, прогоняем ReconcileVideos по всем опубликованным
// спецам один раз.
func (c *Client) CountDocs(ctx context.Context, index string) (int, error) {
	var resp struct {
		Count int `json:"count"`
	}
	if err := c.do(ctx, http.MethodGet, "/"+index+"/_count", nil, &resp); err != nil {
		return 0, err
	}
	return resp.Count, nil
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/_cluster/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return &ErrStatus{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}
