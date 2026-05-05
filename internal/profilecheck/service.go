package profilecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"marketpclce/internal/llm"
)

var (
	ErrLLMDisabled = errors.New("llm_disabled")
	ErrEmptyInput  = errors.New("empty_input")
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
	DisplayName          string
	PrimaryCategory      string
	PrimaryCategoryTitle string
}

type PartResult struct {
	OK         bool     `json:"ok"`
	Score      int      `json:"score"`
	Reasons    []string `json:"reasons"`
	Suggestion string   `json:"suggestion"`
}

type Result struct {
	OK   bool       `json:"ok"`
	Bio  PartResult `json:"bio"`
	Name PartResult `json:"name"`
}

func (s *Service) Available() bool {
	return s != nil && s.client != nil && s.client.HasKey()
}

func (s *Service) Check(ctx context.Context, in Input) (Result, error) {
	bio := strings.TrimSpace(in.Bio)
	name := strings.TrimSpace(in.DisplayName)
	if bio == "" && name == "" {
		return Result{}, ErrEmptyInput
	}
	if !s.Available() {
		return Result{}, ErrLLMDisabled
	}

	out := Result{
		Bio:  skippedPart(),
		Name: skippedPart(),
	}

	var (
		wg              sync.WaitGroup
		bioErr, nameErr error
	)
	if bio != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.checkPart(ctx, buildBioPrompt(in.PrimaryCategory, in.PrimaryCategoryTitle),
				fmt.Sprintf("BIO:\n%s\n\nДлина: %d символов.", bio, utf8.RuneCountInString(bio)))
			if err != nil {
				bioErr = fmt.Errorf("bio: %w", err)
				return
			}
			out.Bio = res
		}()
	}
	if name != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.checkPart(ctx, buildNamePrompt(),
				fmt.Sprintf("DISPLAY_NAME:\n%s\n\nДлина: %d символов.", name, utf8.RuneCountInString(name)))
			if err != nil {
				nameErr = fmt.Errorf("name: %w", err)
				return
			}
			out.Name = res
		}()
	}
	wg.Wait()

	if bioErr != nil {
		return Result{}, bioErr
	}
	if nameErr != nil {
		return Result{}, nameErr
	}

	out.OK = out.Bio.OK && out.Name.OK
	return out, nil
}

func (s *Service) checkPart(ctx context.Context, system, userMsg string) (PartResult, error) {
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
			Format: llm.OutputFormat{Type: "json_schema", Schema: partResponseSchema()},
			Effort: s.effort,
		},
	})
	if err != nil {
		return PartResult{}, fmt.Errorf("llm: %w", err)
	}
	raw := resp.FirstText()
	if raw == "" {
		return PartResult{}, fmt.Errorf("empty response")
	}
	var parsed PartResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return PartResult{}, fmt.Errorf("parse: %w", err)
	}
	return normalizePart(parsed), nil
}

func skippedPart() PartResult {
	return PartResult{OK: true, Score: 100, Reasons: []string{}, Suggestion: ""}
}

func normalizePart(p PartResult) PartResult {
	if p.Score < 0 {
		p.Score = 0
	}
	if p.Score > 100 {
		p.Score = 100
	}
	if p.Reasons == nil {
		p.Reasons = []string{}
	}
	p.Suggestion = strings.TrimSpace(p.Suggestion)
	return p
}
