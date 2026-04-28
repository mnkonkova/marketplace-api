package clarify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"marketpclce/internal/llm"
)

var ErrLLMDisabled = errors.New("llm_disabled")

type Service struct {
	client    llm.Provider
	maxTokens int
	effort    string
}

func NewService(client llm.Provider, maxTokens int, effort string) *Service {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	return &Service{client: client, maxTokens: maxTokens, effort: effort}
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type SearchParams struct {
	Q          string   `json:"q"`
	Categories []string `json:"categories"`
	Skills     []string `json:"skills"`
	City       string   `json:"city"`
	RateMin    *int     `json:"rate_min,omitempty"`
	RateMax    *int     `json:"rate_max,omitempty"`
}

type Result struct {
	Message string        `json:"message"`
	Done    bool          `json:"done"`
	Search  *SearchParams `json:"search,omitempty"`
}

type Input struct {
	Category string
	History  []Message
}

func (s *Service) Run(ctx context.Context, in Input) (Result, error) {
	if s.client == nil || !s.client.HasKey() {
		return Result{}, ErrLLMDisabled
	}

	msgs := make([]llm.Message, 0, len(in.History))
	for _, m := range in.History {
		role := m.Role
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		msgs = append(msgs, llm.Message{Role: role, Content: text})
	}
	if len(msgs) == 0 {
		return Result{}, fmt.Errorf("empty history")
	}

	system := buildSystemPrompt(in.Category)

	resp, err := s.client.Messages(ctx, llm.MessagesRequest{
		MaxTokens: s.maxTokens,
		System: []llm.SystemBlock{{
			Type:         "text",
			Text:         system,
			CacheControl: &llm.CacheControl{Type: "ephemeral"},
		}},
		Messages: msgs,
		Thinking: &llm.Thinking{Type: "adaptive"},
		OutputConfig: &llm.OutputConfig{
			Format: llm.OutputFormat{Type: "json_schema", Schema: responseSchema()},
			Effort: s.effort,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("llm: %w", err)
	}
	raw := resp.FirstText()
	if raw == "" {
		return Result{}, fmt.Errorf("empty response")
	}

	var parsed struct {
		Message string `json:"message"`
		Done    bool   `json:"done"`
		Search  *struct {
			Q          string   `json:"q"`
			Categories []string `json:"categories"`
			Skills     []string `json:"skills"`
			City       string   `json:"city"`
			RateMin    *int     `json:"rate_min"`
			RateMax    *int     `json:"rate_max"`
		} `json:"search"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Result{}, fmt.Errorf("parse: %w", err)
	}

	out := Result{
		Message: strings.TrimSpace(parsed.Message),
		Done:    parsed.Done,
	}
	if parsed.Done && parsed.Search != nil {
		out.Search = &SearchParams{
			Q:          strings.TrimSpace(parsed.Search.Q),
			Categories: dedupNonEmpty(parsed.Search.Categories),
			Skills:     dedupNonEmpty(parsed.Search.Skills),
			City:       strings.TrimSpace(parsed.Search.City),
			RateMin:    parsed.Search.RateMin,
			RateMax:    parsed.Search.RateMax,
		}
		if in.Category != "" {
			out.Search.Categories = ensure(out.Search.Categories, in.Category)
		}
	}
	return out, nil
}

func dedupNonEmpty(xs []string) []string {
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

func ensure(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
