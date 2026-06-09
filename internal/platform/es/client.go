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
	// Инструментация: op резолвится из path/method (см. opFromPath). Если
	// запрос упал до получения статуса (сеть/таймаут) — пишем "error"
	// в status_class, чтобы такие срывы было видно на дашборде отдельно
	// от 5xx.
	op := opFromPath(method, path)
	start := time.Now()
	status := -1
	defer func() {
		esRequestDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
		esRequestsTotal.WithLabelValues(op, statusClass(status)).Inc()
	}()
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
	status = resp.StatusCode

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

// opFromPath — резолвит high-level имя операции из chi-like path и
// метода. Кардинальность ограничена: 8-10 уникальных значений.
// Используется как Prometheus label.
func opFromPath(method, path string) string {
	switch {
	case strings.HasPrefix(path, "/_bulk"):
		return "bulk"
	case strings.HasPrefix(path, "/_cluster"):
		return "cluster_health"
	case strings.Contains(path, "/_search"):
		return "search"
	case strings.Contains(path, "/_count"):
		return "count"
	case strings.Contains(path, "/_delete_by_query"):
		return "delete_by_query"
	case strings.Contains(path, "/_doc/"):
		if method == http.MethodDelete {
			return "delete_doc"
		}
		return "index_doc"
	default:
		// CreateIndex/IndexExists — оба бьются в "/{name}" корнем.
		if method == http.MethodHead {
			return "index_exists"
		}
		if method == http.MethodPut {
			return "create_index"
		}
		return "other"
	}
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

// IndexDocVer — то же что IndexDoc, но с external_gte версией. Если в индексе
// уже есть документ с версией ≥ переданной — OS вернёт 409, который мы
// проглатываем как nil: значит конкурентный воркер уже записал более свежее
// состояние, наша запись была бы регрессом. version=0 → fallback на IndexDoc
// (без version-check, для обратной совместимости).
//
// version обычно = source.updated_at.UnixMicro() в caller'е. Микросекунды
// дают монотонность в пределах одной PG-строки (PG now() строго возрастает)
// и достаточный headroom: 2^63 = ~292 тыс лет.
func (c *Client) IndexDocVer(ctx context.Context, index, id string, doc any, version int64) error {
	if version <= 0 {
		return c.IndexDoc(ctx, index, id, doc)
	}
	path := fmt.Sprintf("/%s/_doc/%s?refresh=false&version=%d&version_type=external_gte", index, id, version)
	err := c.do(ctx, http.MethodPut, path, doc, nil)
	var es *ErrStatus
	if errors.As(err, &es) && es.Status == http.StatusConflict {
		return nil // newer state already in ES, end state achieved
	}
	return err
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
	start := time.Now()
	status := -1
	defer func() {
		esRequestDuration.WithLabelValues("bulk").Observe(time.Since(start).Seconds())
		esRequestsTotal.WithLabelValues("bulk", statusClass(status)).Inc()
	}()
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
	status = resp.StatusCode
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

// DeleteDocVer — то же что DeleteDoc, но с external_gte версией. 409 (текущая
// в индексе версия ≥ нашей) проглатываем как nil: end state, к которому мы
// стремимся (удалить устаревшую версию), уже достигнут более свежей записью
// от конкурентного worker'а. 404 (документа нет) тоже nil. version=0 →
// fallback на DeleteDoc.
//
// version обычно = updated_at.UnixMicro() источника. См. IndexDocVer.
func (c *Client) DeleteDocVer(ctx context.Context, index, id string, version int64) error {
	if version <= 0 {
		return c.DeleteDoc(ctx, index, id)
	}
	path := fmt.Sprintf("/%s/_doc/%s?version=%d&version_type=external_gte", index, id, version)
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	var es *ErrStatus
	if errors.As(err, &es) && (es.Status == http.StatusNotFound || es.Status == http.StatusConflict) {
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
//
// conflicts=proceed — игнорировать version_conflict (409). Когда несколько
// событий specialist.upserted приходят подряд для одного user_id (типично
// при батч-обновлении профиля), delete сканит snapshot v=N, в это время
// bulk index пишет v=N+1 → delete падает 409 при попытке удалить старую
// версию. С proceed просто пропускаем конфликтные документы — следующий
// bulk index в ReconcileVideos их перезапишет, eventual consistency.
func (c *Client) DeleteByQuery(ctx context.Context, index string, query any) error {
	return c.do(ctx, http.MethodPost, "/"+index+"/_delete_by_query?refresh=false&conflicts=proceed", query, nil)
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
