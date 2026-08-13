// Package partner — подтверждение регистрации для «Бота Работ».
//
// Зачем это здесь. У соседнего продукта есть партнёрская цена для тех, кто
// зарегистрирован на маркетплейсе. Проверить регистрацию можно двумя
// способами: поверить человеку на слово («введи почту») или подтвердить
// оттуда, где он уже авторизован. Первый способ раздаёт скидку любому, кто
// знает чужой адрес, поэтому подтверждаем мы.
//
// Что происходит: приложение выдаёт человеку одноразовый код и ведёт его на
// нашу страницу. Здесь он уже залогинен — значит аккаунт точно его, — и мы
// сообщаем об этом «Боту Работ» вебхуком, подписанным общим секретом.
//
// Почему требуем подтверждённую почту. Без этого достаточно зарегистрироваться
// на выдуманный адрес, и скидка получена: проверка превратилась бы в
// формальность, ради которой всё и затевалось.
package partner

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotVerified — почта не подтверждена. Отдельная ошибка, а не общий
	// отказ: человеку надо сказать, что именно сделать, иначе он будет жать
	// кнопку повторно.
	ErrNotVerified = errors.New("email not verified")
	// ErrDisabled — не задан общий секрет. Половина механизма опаснее его
	// отсутствия: без подписи вебхук примет кто угодно.
	ErrDisabled = errors.New("partner linking disabled")
	// ErrRejected — «Бот Работ» не принял код: истёк, уже использован или
	// этот аккаунт уже привязан к другому профилю.
	ErrRejected = errors.New("code rejected")
)

type Service struct {
	db      *pgxpool.Pool
	webhook string
	secret  string
	client  *http.Client
}

func NewService(db *pgxpool.Pool, webhook, secret string) *Service {
	return &Service{
		db:      db,
		webhook: webhook,
		secret:  secret,
		// Таймаут короткий: это шаг в живом сценарии, человек ждёт ответа на
		// экране. Лучше честное «попробуйте ещё раз», чем полминуты спиннера.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *Service) Enabled() bool {
	return s.webhook != "" && s.secret != ""
}

type confirmBody struct {
	Code     string  `json:"code"`
	UserID   string  `json:"user_id"`
	Email    *string `json:"email"`
	IssuedAt string  `json:"issued_at"`
}

// Link подтверждает код: проверяет, что почта подтверждена, и сообщает об
// этом «Боту Работ».
func (s *Service) Link(ctx context.Context, userID uuid.UUID, code string) error {
	if !s.Enabled() {
		return ErrDisabled
	}

	var email *string
	var verifiedAt *time.Time
	const q = `SELECT email, email_verified_at FROM users WHERE id = $1 AND is_active`
	if err := s.db.QueryRow(ctx, q, userID).Scan(&email, &verifiedAt); err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if verifiedAt == nil {
		return ErrNotVerified
	}

	// Время в теле — часть подписи: без него перехваченный запрос можно было
	// бы повторить когда угодно.
	body, err := json.Marshal(confirmBody{
		Code:     code,
		UserID:   userID.String(),
		Email:    email,
		IssuedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sign(s.secret, body))

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("call botrabot: %w", err)
	}
	defer resp.Body.Close()
	// Тело читаем всегда: без этого соединение не переиспользуется, а на
	// ошибках оно ещё и содержит причину, которую видно в логах.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Код истёк, уже использован или аккаунт занят — виноват не сбой, а
		// ситуация, и рассказать о ней должен экран, а не пятисотка.
		return fmt.Errorf("%w: %s: %s", ErrRejected, resp.Status, payload)
	default:
		return fmt.Errorf("botrabot answered %s: %s", resp.Status, payload)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignForTest открывает подпись тестам. Отдельная функция, а не экспорт sign:
// подписывание — внутреннее дело сервиса, и звать его из другого кода незачем.
func SignForTest(secret string, body []byte) string { return sign(secret, body) }
