package telegram

import (
	"context"
	"crypto/hmac"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service — привязки к ботам: коды из кабинета, ответы бота, отбор
// получателей.
//
// Проверка initData живёт отдельно (verify.go) и сервису не нужна: её
// зовёт auth, когда решает, кто пришёл.
type Service struct {
	repo *Repo
	now  func() time.Time
	// usernames — под каким именем бот живёт в Telegram. Нужно ровно
	// для одного: собрать ссылку t.me/<bot>?start=<код>. Пусто —
	// ссылку не отдаём: «t.me/?start=…» ведёт в никуда, и человек
	// решит, что сломались мы.
	creatorUsername string
	clientUsername  string
	// sharedSecret — общий секрет для входящих /bot/*. Пусто — группа
	// ручек не поднимается вовсе.
	sharedSecret string
	// dailyCap — потолок сообщений в сутки на человека.
	dailyCap int
}

// DailyCap — сколько сообщений в сутки допустимо одному человеку.
//
// Не про технику: тот, кому за день пришло тридцать писем, перестаёт
// читать тридцать первое, а заодно и все остальные наши. Считается по
// общему notification_log — второй счётчик разошёлся бы с ним на
// первом же перезапуске.
const DailyCap = 30

func NewService(repo *Repo) *Service {
	return &Service{repo: repo, now: time.Now, dailyCap: DailyCap}
}

// WithBots — имена ботов и общий секрет входящих ручек.
func (s *Service) WithBots(creatorUsername, clientUsername, sharedSecret string) *Service {
	s.creatorUsername = creatorUsername
	s.clientUsername = clientUsername
	s.sharedSecret = sharedSecret
	return s
}

// BotUsername — имя бота в Telegram. Пусто = не настроен.
func (s *Service) BotUsername(bot string) string {
	switch bot {
	case BotCreator:
		return s.creatorUsername
	case BotClient:
		return s.clientUsername
	default:
		return ""
	}
}

// CheckSecret — общий секрет входящих ручек бота.
//
// hmac.Equal, а не ==: сравнение строк выходит за первым несовпавшим
// байтом и рассказывает подбирающему, насколько он близок.
func (s *Service) CheckSecret(got string) bool {
	if s.sharedSecret == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(s.sharedSecret))
}

// Enabled — входящие ручки бота подняты.
func (s *Service) Enabled() bool { return s.sharedSecret != "" }

// LinkStart — ссылка, по которой человек подключает бота.
//
// Код одноразовый и живёт пятнадцать минут: человек нажимает кнопку и
// тут же идёт в Telegram, а утёкшая долгоживущая ссылка — это ключ от
// аккаунта.
type LinkStart struct {
	Bot string `json:"bot"`
	// URL — то, что открывают. Код внутри, отдельно его показывать
	// незачем: руками его никто не вводит.
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewLinkStart — выдать ссылку подключения.
func (s *Service) NewLinkStart(ctx context.Context, userID uuid.UUID, bot string) (LinkStart, error) {
	username := s.BotUsername(bot)
	if !KnownBot(bot) || username == "" {
		return LinkStart{}, ErrUnknownBot
	}
	now := s.now()
	code, err := s.repo.NewCode(ctx, userID, bot, now)
	if err != nil {
		return LinkStart{}, err
	}
	return LinkStart{
		Bot:       bot,
		URL:       fmt.Sprintf("https://t.me/%s?start=%s", username, code),
		ExpiresAt: now.Add(CodeTTL),
	}, nil
}

// NewTicket — билет привязки для того, у кого аккаунта ещё нет.
//
// Мини-апп кладёт его в адрес анкеты, браузер гасит сразу после
// регистрации. Так человек не возвращается в бот нажимать что-то ещё:
// он уже получил, зачем приходил, и половина не вернулась бы.
func (s *Service) NewTicket(
	ctx context.Context, bot string, tgUserID, tgChatID int64, username string,
) (string, error) {
	return s.repo.NewTicket(ctx, bot, tgUserID, tgChatID, username, s.now())
}

// Claim — предъявить билет из-под свежей сессии.
func (s *Service) Claim(ctx context.Context, userID uuid.UUID, code string) (Link, error) {
	if code == "" {
		return Link{}, ErrNotFound
	}
	return s.repo.ClaimTicket(ctx, userID, code, s.now())
}

// Status — что показывает кабинет: привязки и доступные боты.
type Status struct {
	Links []Link `json:"links"`
	// Available — какие боты вообще настроены. Кнопку «подключить»
	// рисуем только по ним: кнопка, ведущая в t.me/?start=, хуже
	// отсутствующей.
	Available []string `json:"available"`
}

func (s *Service) Status(ctx context.Context, userID uuid.UUID) (Status, error) {
	links, err := s.repo.ListForUser(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	out := Status{Links: links, Available: make([]string, 0, 2)}
	for _, bot := range []string{BotCreator, BotClient} {
		if s.BotUsername(bot) != "" {
			out.Available = append(out.Available, bot)
		}
	}
	return out, nil
}

// LinkByCode — бот погасил код и привязал человека.
func (s *Service) LinkByCode(
	ctx context.Context, bot, code string, tgUserID, tgChatID int64, username string,
) (Link, error) {
	if tgUserID == 0 {
		return Link{}, fmt.Errorf("%w: пустой tg_user_id", ErrNotFound)
	}
	if tgChatID == 0 {
		// В личке chat_id совпадает с id пользователя; если бот его не
		// прислал — берём то, что знаем, вместо отказа.
		tgChatID = tgUserID
	}
	return s.repo.LinkByCode(ctx, bot, code, tgUserID, tgChatID, username, s.now())
}

// ByTelegram — «кто это»: бот знает только tg_user_id.
func (s *Service) ByTelegram(ctx context.Context, bot string, tgUserID int64) (Link, error) {
	return s.repo.ByTelegram(ctx, bot, tgUserID)
}

// Blocked — человек заблокировал бота.
func (s *Service) Blocked(ctx context.Context, bot string, tgUserID int64) (Link, error) {
	return s.repo.MarkBlocked(ctx, bot, tgUserID, s.now())
}

// Unlink — отключить бота из кабинета.
func (s *Service) Unlink(ctx context.Context, userID uuid.UUID, bot string) error {
	if !KnownBot(bot) {
		return ErrUnknownBot
	}
	return s.repo.Unlink(ctx, userID, bot)
}

// Recipients — кому из этих людей можно написать в этот бот сегодня.
func (s *Service) Recipients(ctx context.Context, bot string, userIDs []uuid.UUID) ([]Recipient, error) {
	if !KnownBot(bot) {
		return nil, ErrUnknownBot
	}
	day := s.now().UTC().Truncate(24 * time.Hour)
	return s.repo.Recipients(ctx, bot, userIDs, day, s.dailyCap)
}

// LeaseMessages — выдать боту пачку неотправленных сообщений.
func (s *Service) LeaseMessages(ctx context.Context, limit int) ([]Message, error) {
	return s.repo.Lease(ctx, limit)
}

// AckMessages — что бот с пачкой сделал.
func (s *Service) AckMessages(ctx context.Context, delivered []int64, failed map[int64]string) error {
	return s.repo.Ack(ctx, delivered, failed)
}
