package profilecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"marketpclce/internal/llm"
)

var (
	ErrLLMDisabled = errors.New("llm_disabled")
	ErrEmptyBio    = errors.New("empty_bio")
)

const PublishMinScore = 60

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

type Input struct {
	Bio                  string
	PrimaryCategory      string
	PrimaryCategoryTitle string
}

type Result struct {
	OK         bool     `json:"ok"`
	Score      int      `json:"score"`
	Reasons    []string `json:"reasons"`
	Suggestion string   `json:"suggestion"`
}

func (s *Service) Available() bool {
	return s != nil && s.client != nil && s.client.HasKey()
}

func (s *Service) Check(ctx context.Context, in Input) (Result, error) {
	bio := strings.TrimSpace(in.Bio)
	if bio == "" {
		return Result{}, ErrEmptyBio
	}
	if !s.Available() {
		return Result{}, ErrLLMDisabled
	}

	system := buildSystemPrompt(in.PrimaryCategory, in.PrimaryCategoryTitle)

	userMsg := fmt.Sprintf("BIO:\n%s\n\nДлина: %d символов.", bio, utf8.RuneCountInString(bio))

	resp, err := s.client.Messages(ctx, llm.MessagesRequest{
		MaxTokens: s.maxTokens,
		System: []llm.SystemBlock{{
			Type:         "text",
			Text:         system,
			CacheControl: &llm.CacheControl{Type: "ephemeral"},
		}},
		Messages: []llm.Message{{Role: "user", Content: userMsg}},
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

	var parsed Result
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Result{}, fmt.Errorf("parse: %w", err)
	}
	if parsed.Score < 0 {
		parsed.Score = 0
	}
	if parsed.Score > 100 {
		parsed.Score = 100
	}
	if parsed.Reasons == nil {
		parsed.Reasons = []string{}
	}
	parsed.Suggestion = strings.TrimSpace(parsed.Suggestion)
	return parsed, nil
}
