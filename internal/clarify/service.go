package clarify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"marketpclce/internal/llm"
)

var ErrLLMDisabled = errors.New("llm_disabled")

// CategoryLister — источник актуального списка категорий для system-промпта.
// Реализуется тонким адаптером поверх catalog.Repo на стороне main, чтобы
// не вводить здесь обратную зависимость на catalog.
type CategoryLister interface {
	ListCategoriesForPrompt(ctx context.Context) ([]CategoryRef, error)
}

// SkillLister — источник актуального словаря навыков. Принимает category:
// пустая строка → все навыки + платформы (используется когда категория ещё
// не выбрана); код → только навыки этой категории плюс платформы (они
// фасет, общий для всех категорий).
type SkillLister interface {
	ListSkillsForPrompt(ctx context.Context, category string) ([]SkillRef, error)
}

const promptCacheTTL = 5 * time.Minute

type Service struct {
	client    llm.Provider
	maxTokens int
	effort    string

	lister      CategoryLister
	skillLister SkillLister

	catMu     sync.Mutex
	catCache  []CategoryRef
	catExpiry time.Time

	skMu     sync.Mutex
	skCache  map[string][]SkillRef
	skExpiry map[string]time.Time
}

func NewService(client llm.Provider, maxTokens int, effort string) *Service {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	return &Service{
		client:    client,
		maxTokens: maxTokens,
		effort:    effort,
		skCache:   map[string][]SkillRef{},
		skExpiry:  map[string]time.Time{},
	}
}

// WithCategoryLister включает динамическую подгрузку категорий из БД.
// Без вызова — Service использует fallbackCategories из prompt.go.
func (s *Service) WithCategoryLister(l CategoryLister) *Service {
	s.lister = l
	return s
}

// WithSkillLister включает динамическую подгрузку словаря навыков из БД.
// Без вызова — Service использует fallbackSkills из prompt.go.
func (s *Service) WithSkillLister(l SkillLister) *Service {
	s.skillLister = l
	return s
}

// categories возвращает кешированный список категорий (TTL 5 минут).
// Если lister упал или не настроен — возвращает nil; вызывающий код
// должен передать nil в buildSystemPrompt, тот подставит fallbackCategories.
func (s *Service) categories(ctx context.Context) []CategoryRef {
	if s.lister == nil {
		return nil
	}
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if time.Now().Before(s.catExpiry) && s.catCache != nil {
		return s.catCache
	}
	cats, err := s.lister.ListCategoriesForPrompt(ctx)
	if err != nil {
		slog.Warn("clarify: list categories for prompt failed, using fallback", "err", err)
		return nil
	}
	s.catCache = cats
	s.catExpiry = time.Now().Add(promptCacheTTL)
	return cats
}

// skills возвращает кешированный словарь навыков для категории (TTL 5 минут).
// Кеш по ключу category, чтобы фильтр был дешёвым на горячих категориях.
// Если lister упал или не настроен — возвращает nil, и buildSystemPrompt
// подставит fallbackSkills.
func (s *Service) skills(ctx context.Context, category string) []SkillRef {
	if s.skillLister == nil {
		return nil
	}
	s.skMu.Lock()
	defer s.skMu.Unlock()
	if exp, ok := s.skExpiry[category]; ok && time.Now().Before(exp) {
		return s.skCache[category]
	}
	skills, err := s.skillLister.ListSkillsForPrompt(ctx, category)
	if err != nil {
		slog.Warn("clarify: list skills for prompt failed, using fallback", "err", err, "category", category)
		return nil
	}
	s.skCache[category] = skills
	s.skExpiry[category] = time.Now().Add(promptCacheTTL)
	return skills
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

	system := buildSystemPrompt(s.categories(ctx), s.skills(ctx, in.Category), in.Category)

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
			Categories: DedupNonEmpty(parsed.Search.Categories),
			Skills:     DedupNonEmpty(parsed.Search.Skills),
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

func DedupNonEmpty(xs []string) []string {
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
