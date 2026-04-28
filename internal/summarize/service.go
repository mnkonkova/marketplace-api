package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"marketpclce/internal/llm"
	"marketpclce/internal/search"
)

type Service struct {
	search    *search.Service
	client    llm.Provider
	maxTokens int
	effort    string
}

func NewService(searchSvc *search.Service, client llm.Provider, maxTokens int, effort string) *Service {
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	return &Service{search: searchSvc, client: client, maxTokens: maxTokens, effort: effort}
}

type Pick struct {
	UserID  string          `json:"user_id"`
	Rank    int             `json:"rank"`
	Reason  string          `json:"reason"`
	Profile search.IndexDoc `json:"profile"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type Result struct {
	Summary         string `json:"summary"`
	Picks           []Pick `json:"picks"`
	Usage           Usage  `json:"usage"`
	Cached          bool   `json:"cached,omitempty"`
	Broadened       bool   `json:"broadened,omitempty"`
	TargetCategory  string `json:"target_category,omitempty"`
	TotalInCategory int    `json:"total_in_category,omitempty"`
}

type parsedResponse struct {
	Summary string `json:"summary"`
	Picks   []struct {
		UserID string `json:"user_id"`
		Rank   int    `json:"rank"`
		Reason string `json:"reason"`
	} `json:"picks"`
}

func (s *Service) Run(ctx context.Context, q search.Query) (Result, error) {
	if q.Limit <= 0 || q.Limit > 30 {
		q.Limit = 20
	}

	res, err := s.search.Search(ctx, q)
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	if len(res.Items) == 0 {
		return Result{
			Summary: "По заданным критериям ничего не нашлось в каталоге. Попробуйте смягчить фильтры или переформулировать запрос.",
			Picks:   []Pick{},
		}, nil
	}

	userMsg := buildUserMessage(q, res.Items)

	resp, err := s.client.Messages(ctx, llm.MessagesRequest{
		MaxTokens: s.maxTokens,
		System: []llm.SystemBlock{{
			Type:         "text",
			Text:         systemPrompt,
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
		return Result{}, fmt.Errorf("llm empty response")
	}

	var parsed parsedResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Result{}, fmt.Errorf("parse llm response: %w", err)
	}

	byID := make(map[string]search.IndexDoc, len(res.Items))
	for _, c := range res.Items {
		byID[c.UserID] = c
	}

	out := Result{
		Summary:   strings.TrimSpace(parsed.Summary),
		Picks:     make([]Pick, 0, len(parsed.Picks)),
		Broadened: res.Broadened,
		Usage: Usage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
		},
	}
	for _, p := range parsed.Picks {
		doc, ok := byID[p.UserID]
		if !ok {
			continue
		}
		out.Picks = append(out.Picks, Pick{
			UserID:  p.UserID,
			Rank:    p.Rank,
			Reason:  strings.TrimSpace(p.Reason),
			Profile: doc,
		})
	}
	sort.SliceStable(out.Picks, func(i, j int) bool { return out.Picks[i].Rank < out.Picks[j].Rank })
	return out, nil
}

func buildUserMessage(q search.Query, cands []search.IndexDoc) string {
	var sb strings.Builder
	sb.WriteString("Запрос клиента: ")
	if strings.TrimSpace(q.Q) == "" {
		sb.WriteString("(свободный запрос не задан)")
	} else {
		sb.WriteString(q.Q)
	}
	sb.WriteString("\n")

	var filters []string
	if len(q.Categories) > 0 {
		filters = append(filters, "категории: "+strings.Join(q.Categories, ", "))
	}
	if len(q.SkillSlugs) > 0 {
		filters = append(filters, "навыки: "+strings.Join(q.SkillSlugs, ", "))
	}
	if q.City != "" {
		filters = append(filters, "город: "+q.City)
	}
	if q.RateMin != nil || q.RateMax != nil {
		rmin := "—"
		rmax := "—"
		if q.RateMin != nil {
			rmin = strconv.Itoa(*q.RateMin)
		}
		if q.RateMax != nil {
			rmax = strconv.Itoa(*q.RateMax)
		}
		filters = append(filters, "бюджет: "+rmin+" — "+rmax)
	}
	if len(filters) > 0 {
		sb.WriteString("Фильтры: ")
		sb.WriteString(strings.Join(filters, "; "))
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\nКандидаты (%d):\n", len(cands)))

	short := make([]map[string]any, 0, len(cands))
	for _, c := range cands {
		entry := map[string]any{
			"user_id":          c.UserID,
			"name":             c.DisplayName,
			"primary_category": c.PrimaryCategory,
			"categories":       c.Categories,
			"skills":           c.SkillTitles,
			"city":             c.City,
			"currency":         c.Currency,
			"rating":           c.RatingAvg,
			"reviews":          c.ReviewsCount,
			"bio":              truncRunes(c.Bio, 200),
		}
		if c.RateMin != nil {
			entry["rate_min"] = *c.RateMin
		}
		if c.RateMax != nil {
			entry["rate_max"] = *c.RateMax
		}
		short = append(short, entry)
	}
	enc, _ := json.Marshal(short)
	sb.Write(enc)
	return sb.String()
}

func truncRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
