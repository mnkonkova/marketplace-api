package llm

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

const (
	anthropicVersion = "2023-06-01"
	defaultModel     = "claude-sonnet-4-6"
)

type Anthropic struct {
	base   string
	apiKey string
	model  string
	http   *http.Client
}

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

func NewAnthropic(c Config) *Anthropic {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	model := c.Model
	if model == "" {
		model = defaultModel
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Anthropic{
		base:   base,
		apiKey: c.APIKey,
		model:  model,
		http:   &http.Client{Timeout: timeout},
	}
}

type CacheControl struct {
	Type string `json:"type"`
}

type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Thinking struct {
	Type    string `json:"type"`
	Display string `json:"display,omitempty"`
}

type OutputFormat struct {
	Type   string `json:"type"`
	Schema any    `json:"schema,omitempty"`
}

type OutputConfig struct {
	Format OutputFormat `json:"format"`
	Effort string       `json:"effort,omitempty"`
}

type MessagesRequest struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	System       []SystemBlock  `json:"system,omitempty"`
	Messages     []Message      `json:"messages"`
	Thinking     *Thinking      `json:"thinking,omitempty"`
	OutputConfig *OutputConfig  `json:"output_config,omitempty"`
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []ContentBlock `json:"content"`
	Usage      Usage          `json:"usage"`
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("anthropic %d: %s", e.Status, e.Body) }

func (a *Anthropic) Model() string { return a.model }

func (a *Anthropic) HasKey() bool { return a != nil && a.apiKey != "" }

func (a *Anthropic) Messages(ctx context.Context, req MessagesRequest) (*MessagesResponse, error) {
	if a.apiKey == "" {
		return nil, errors.New("anthropic api key not configured")
	}
	if req.Model == "" {
		req.Model = a.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic call: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var out MessagesResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

func (r *MessagesResponse) FirstText() string {
	for _, b := range r.Content {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}
