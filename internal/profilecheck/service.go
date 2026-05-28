package profilecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
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
	req := llm.MessagesRequest{
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
	}

	// Один ретрай на транзиентных сбоях LLM-провайдера: сетевые ошибки,
	// таймауты, 5xx/429. Холодный коннект к deepseek/anthropic иногда зависает
	// дольше HTTP-таймаута, а publish без чекера падает 500 — это плохой UX.
	// Парс-ошибки и 4xx (кроме 429) не ретраим: LLM вернул что-то осмысленное
	// и проблема не уйдёт от повтора.
	var (
		resp *llm.MessagesResponse
		err  error
	)
	for attempt := 0; attempt < 2; attempt++ {
		resp, err = s.client.Messages(ctx, req)
		if err == nil {
			break
		}
		if !isTransientLLMError(err) || ctx.Err() != nil {
			break
		}
		slog.Warn("profilecheck llm transient, retrying", "attempt", attempt+1, "err", err)
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return PartResult{}, fmt.Errorf("llm: %w", ctx.Err())
		}
	}
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

// isTransientLLMError — стоит ли повторить запрос. Берём:
//   - context.DeadlineExceeded (наш HTTP-таймаут на клиенте);
//   - net.Error (включая io timeout, connection reset, EOF от прокси);
//   - llm.APIError с 5xx / 408 / 429.
//
// Не берём 4xx (кроме 408/429) и context.Canceled (юзер ушёл — повторять
// бессмысленно).
func isTransientLLMError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500 || apiErr.Status == 408 || apiErr.Status == 429
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
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
