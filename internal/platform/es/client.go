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
