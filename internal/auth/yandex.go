package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Вход через Яндекс.
//
// Схема: фронт уводит человека на oauth.yandex.ru, тот возвращает браузер на
// redirect_uri с одноразовым `code`. Фронт отдаёт код сюда, обмен на токен
// делает бэкенд — client_secret на клиент не попадает.
//
// Код в адресной строке жить может: он одноразовый и живёт минуты. А вот
// наши access/refresh через URL не передаём — они осели бы в истории
// браузера и логах прокси, поэтому фронт получает их ответом на POST.

var (
	// ErrYandexDisabled — ключи не заданы. Локальный запуск без OAuth это
	// норма, поэтому отличаем от настоящей ошибки.
	ErrYandexDisabled = errors.New("yandex oauth is not configured")
	ErrYandexExchange = errors.New("yandex code exchange failed")
)

type YandexConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func (c YandexConfig) Enabled() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// YandexProfile — то, что нам нужно от Яндекса: кто это и как его зовут.
type YandexProfile struct {
	ID           string `json:"id"`
	DefaultEmail string `json:"default_email"`
	DisplayName  string `json:"display_name"`
	RealName     string `json:"real_name"`
	Login        string `json:"login"`
}

// Name — человекочитаемое имя с запасными вариантами: display_name у
// Яндекса заполнен не всегда.
func (p YandexProfile) Name() string {
	for _, v := range []string{p.DisplayName, p.RealName, p.Login} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

type yandexClient struct {
	cfg  YandexConfig
	http *http.Client
}

func newYandexClient(cfg YandexConfig) *yandexClient {
	// Таймаут короткий: человек ждёт в браузере, а зависший запрос к
	// стороннему сервису держал бы наш обработчик.
	return &yandexClient{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// exchange меняет одноразовый код на access-токен Яндекса.
func (c *yandexClient) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
	}
	// redirect_uri обязателен, если он был в запросе авторизации.
	if c.cfg.RedirectURI != "" {
		form.Set("redirect_uri", c.cfg.RedirectURI)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth.yandex.ru/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrYandexExchange, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: bad response", ErrYandexExchange)
	}
	if out.AccessToken == "" {
		// Текст ошибки Яндекса наружу не отдаём — он про наш client_secret и
		// человеку ничего не говорит; логируем на уровне обработчика.
		return "", fmt.Errorf("%w: %s %s", ErrYandexExchange, out.Error, out.ErrorDescription)
	}
	return out.AccessToken, nil
}

// profile забирает данные пользователя по access-токену Яндекса.
func (c *yandexClient) profile(ctx context.Context, token string) (YandexProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://login.yandex.ru/info?format=json", nil)
	if err != nil {
		return YandexProfile{}, err
	}
	req.Header.Set("Authorization", "OAuth "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return YandexProfile{}, fmt.Errorf("%w: %v", ErrYandexExchange, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return YandexProfile{}, fmt.Errorf("%w: profile status %d", ErrYandexExchange, resp.StatusCode)
	}
	var p YandexProfile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return YandexProfile{}, fmt.Errorf("%w: bad profile", ErrYandexExchange)
	}
	if p.ID == "" {
		return YandexProfile{}, fmt.Errorf("%w: empty profile id", ErrYandexExchange)
	}
	return p, nil
}
