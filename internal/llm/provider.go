package llm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Provider interface {
	HasKey() bool
	Model() string
	Messages(ctx context.Context, req MessagesRequest) (*MessagesResponse, error)
}

type ProviderConfig struct {
	Name    string
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

func NewProvider(c ProviderConfig) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(c.Name)) {
	case "", "anthropic":
		return NewAnthropic(Config{
			BaseURL: defaultIfEmpty(c.BaseURL, "https://api.anthropic.com"),
			APIKey:  c.APIKey,
			Model:   defaultIfEmpty(c.Model, "claude-sonnet-4-6"),
			Timeout: c.Timeout,
		}), nil
	case "deepseek":
		return NewDeepSeek(Config{
			BaseURL: defaultIfEmpty(c.BaseURL, "https://api.deepseek.com"),
			APIKey:  c.APIKey,
			Model:   defaultIfEmpty(c.Model, "deepseek-chat"),
			Timeout: c.Timeout,
		}), nil
	default:
		return nil, fmt.Errorf("unknown LLM provider: %q", c.Name)
	}
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
