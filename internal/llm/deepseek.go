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

type DeepSeek struct {
	base   string
	apiKey string
	model  string
	http   *http.Client
}

func NewDeepSeek(c Config) *DeepSeek {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.deepseek.com"
	}
	model := c.Model
	if model == "" {
		model = "deepseek-chat"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &DeepSeek{
		base:   base,
		apiKey: c.APIKey,
		model:  model,
		http:   &http.Client{Timeout: timeout},
	}
}

func (d *DeepSeek) Model() string { return d.model }

func (d *DeepSeek) HasKey() bool { return d != nil && d.apiKey != "" }

type dsMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type dsResponseFormat struct {
	Type string `json:"type"`
}

type dsRequest struct {
	Model          string            `json:"model"`
	Messages       []dsMessage       `json:"messages"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	ResponseFormat *dsResponseFormat `json:"response_format,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
}

type dsChoice struct {
	Index   int       `json:"index"`
	Message dsMessage `json:"message"`
}

type dsUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

type dsResponse struct {
	ID      string     `json:"id"`
	Model   string     `json:"model"`
	Choices []dsChoice `json:"choices"`
	Usage   dsUsage    `json:"usage"`
}

func (d *DeepSeek) Messages(ctx context.Context, req MessagesRequest) (*MessagesResponse, error) {
	if d.apiKey == "" {
		return nil, errors.New("deepseek api key not configured")
	}

	model := req.Model
	if model == "" {
		model = d.model
	}

	systemText := buildSystemText(req)
	wantJSON := req.OutputConfig != nil && (req.OutputConfig.Format.Type == "json_schema" || req.OutputConfig.Format.Type == "json_object")
	if wantJSON {
		systemText = appendJSONInstruction(systemText, req.OutputConfig.Format.Schema)
	}

	msgs := make([]dsMessage, 0, len(req.Messages)+1)
	if systemText != "" {
		msgs = append(msgs, dsMessage{Role: "system", Content: systemText})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, dsMessage{Role: m.Role, Content: m.Content})
	}

	body := dsRequest{Model: model, Messages: msgs, MaxTokens: req.MaxTokens}
	if wantJSON {
		body.ResponseFormat = &dsResponseFormat{Type: "json_object"}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+d.apiKey)

	resp, err := d.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek call: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(respBody)}
	}

	var ds dsResponse
	if err := json.Unmarshal(respBody, &ds); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	out := &MessagesResponse{
		ID:    ds.ID,
		Model: ds.Model,
		Usage: Usage{
			InputTokens:          ds.Usage.PromptTokens,
			OutputTokens:         ds.Usage.CompletionTokens,
			CacheReadInputTokens: ds.Usage.PromptCacheHitTokens,
		},
	}
	if len(ds.Choices) > 0 {
		out.Content = []ContentBlock{{Type: "text", Text: ds.Choices[0].Message.Content}}
	}
	return out, nil
}

func buildSystemText(req MessagesRequest) string {
	parts := make([]string, 0, len(req.System))
	for _, b := range req.System {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n")
}

func appendJSONInstruction(system string, schema any) string {
	var sb strings.Builder
	if system != "" {
		sb.WriteString(system)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Отвечай строго в формате JSON, без обрамляющего текста и markdown.")
	if schema != nil {
		if raw, err := json.Marshal(schema); err == nil {
			sb.WriteString(" JSON-схема ответа: ")
			sb.Write(raw)
		}
	}
	return sb.String()
}
