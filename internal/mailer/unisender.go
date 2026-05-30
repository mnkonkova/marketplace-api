package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UnisenderGo — реализация Sender через REST API Unisender Go
// (https://godoc.unisender.com/). Один запрос на письмо, без батчей —
// для transactional verify-email это нормально.
type UnisenderGo struct {
	apiKey    string
	baseURL   string
	fromEmail string
	fromName  string
	http      *http.Client
}

type UnisenderGoConfig struct {
	APIKey    string
	BaseURL   string // напр. https://go1.unisender.ru/ru/transactional/api/v1
	FromEmail string
	FromName  string
}

func NewUnisenderGo(cfg UnisenderGoConfig) *UnisenderGo {
	return &UnisenderGo{
		apiKey:    cfg.APIKey,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		fromEmail: cfg.FromEmail,
		fromName:  cfg.FromName,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// Тело запроса соответствует /email/send.json:
//
//	{
//	  "message": {
//	    "recipients": [{"email": "...", "substitutions": {"to_name": "..."}}],
//	    "subject": "...",
//	    "body": {"plaintext": "...", "html": "..."},
//	    "from_email": "...",
//	    "from_name": "..."
//	  }
//	}
type unisenderBody struct {
	Message unisenderMessage `json:"message"`
}

type unisenderMessage struct {
	Recipients []unisenderRecipient `json:"recipients"`
	Subject    string               `json:"subject"`
	Body       unisenderMessageBody `json:"body"`
	FromEmail  string               `json:"from_email"`
	FromName   string               `json:"from_name"`
}

type unisenderRecipient struct {
	Email         string            `json:"email"`
	Substitutions map[string]string `json:"substitutions,omitempty"`
}

type unisenderMessageBody struct {
	Plaintext string `json:"plaintext"`
	HTML      string `json:"html,omitempty"`
}

func (u *UnisenderGo) Send(ctx context.Context, m Message) error {
	if u.apiKey == "" {
		return fmt.Errorf("unisender: api key not configured")
	}
	if u.fromEmail == "" {
		return fmt.Errorf("unisender: from_email not configured")
	}

	rcpt := unisenderRecipient{Email: m.To}
	if m.ToName != "" {
		rcpt.Substitutions = map[string]string{"to_name": m.ToName}
	}
	body := unisenderBody{Message: unisenderMessage{
		Recipients: []unisenderRecipient{rcpt},
		Subject:    m.Subject,
		Body:       unisenderMessageBody{Plaintext: m.Plain, HTML: m.HTML},
		FromEmail:  u.fromEmail,
		FromName:   u.fromName,
	}}

	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("unisender: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.baseURL+"/email/send.json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", u.apiKey)

	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("unisender: http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("unisender: %d %s", resp.StatusCode, string(respBody))
	}
	return nil
}
