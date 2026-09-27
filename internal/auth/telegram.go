package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"marketpclce/internal/outbox"
	"marketpclce/internal/telegram"
)

// Вход из мини-аппа Telegram.
//
// Отличается от входа через Яндекс одним, но важным: Telegram не даёт
// ни почты, ни телефона. Человек приходит с одним id — и этого
// достаточно, чтобы его опознать (подпись проверяется нашим же токеном
// бота), но недостаточно, чтобы написать ему письмо. Поэтому такой
// аккаунт живёт без почты, а проверка «человека есть чем опознать»
// перестала быть проверкой почты.
//
// Молча новый аккаунт не заводим: у человека может быть наш аккаунт с
// паролем, и второй, пустой, оставил бы его без проектов. Мини-апп
// сперва спрашивает «у меня уже есть / я новый», и create=false —
// именно этот вопрос: не нашли — ответили not_found, а не завели.

// ProviderTelegram — провайдер в user_identities. «Кто человек» живёт
// там же, где Яндекс; telegram_links отвечает только за доставку.
const ProviderTelegram = "telegram"

var (
	// ErrTelegramDisabled — бот не настроен: токена нет.
	ErrTelegramDisabled = errors.New("telegram bot is not configured")
	// ErrTelegramUnknown — этот телеграм у нас не встречался, а
	// заводить аккаунт не просили.
	ErrTelegramUnknown = errors.New("telegram account is not linked to any user")
)

// TelegramConfig — токены обоих ботов и срок жизни подписи.
type TelegramConfig struct {
	CreatorToken string
	ClientToken  string
	TTL          time.Duration
}

// Token — токен бота по его имени. Пусто — бот выключен.
func (c TelegramConfig) Token(bot string) string {
	switch bot {
	case telegram.BotCreator:
		return c.CreatorToken
	case telegram.BotClient:
		return c.ClientToken
	default:
		return ""
	}
}

// TelegramLinker — кто записывает привязку к боту. Интерфейсом, чтобы
// auth не тащил за собой хранилище доставки: ему нужно одно действие.
type TelegramLinker interface {
	LinkDirect(
		ctx context.Context, userID uuid.UUID, bot string,
		tgUserID, tgChatID int64, username string, now time.Time,
	) (telegram.Link, error)
}

// WithTelegram — включить вход из мини-аппа.
func (s *Service) WithTelegram(cfg TelegramConfig, links TelegramLinker) *Service {
	s.telegram = cfg
	s.tgLinks = links
	return s
}

// TelegramEnabled — есть ли хоть один настроенный бот. По этому флагу
// фронт решает, показывать ли вход из мини-аппа.
func (s *Service) TelegramEnabled() bool {
	return s.telegram.CreatorToken != "" || s.telegram.ClientToken != ""
}

// TelegramLogin — что просят у входа из мини-аппа.
type TelegramLogin struct {
	Bot string
	// InitData — СЫРАЯ строка Telegram.WebApp.initData. Пересобирать её
	// на клиенте нельзя: подпись считается по тому, что прислал
	// Telegram, и любая нормализация ломает её без следа.
	InitData string
	// Create — завести аккаунт, если этого телеграма у нас нет. Ответ
	// на вопрос мини-аппа «у меня уже есть аккаунт / я новый».
	Create bool
	// Login/Password — «у меня уже есть»: привязываем к существующему.
	Login    string
	Password string
}

// LoginWithTelegram — вход или регистрация из мини-аппа.
func (s *Service) LoginWithTelegram(ctx context.Context, in TelegramLogin) (RegisterResult, error) {
	token := s.telegram.Token(in.Bot)
	if !telegram.KnownBot(in.Bot) || token == "" {
		return RegisterResult{}, ErrTelegramDisabled
	}
	data, err := telegram.Verify(in.InitData, token, s.telegram.TTL, s.now())
	if err != nil {
		return RegisterResult{}, err
	}
	tgID := data.User.ID
	providerID := strconv.FormatInt(tgID, 10)

	// 1. Этот телеграм уже знаком — просто впускаем. Привязку
	// освежаем: chat_id и юзернейм меняются, а писать надо в живой.
	id, kind, err := s.repo.FindByIdentity(ctx, ProviderTelegram, providerID)
	switch {
	case err == nil:
		kind, err = s.upgradeKindForBot(ctx, id, kind, in.Bot)
		if err != nil {
			return RegisterResult{}, err
		}
		s.rememberLink(ctx, id, in.Bot, data.User)
		pair, err := s.tokens.Issue(id, s.now())
		if err == nil {
			s.touchLastLogin(ctx, id)
		}
		return RegisterResult{UserID: id, Tokens: pair, Kind: kind}, err
	case !errors.Is(err, ErrNotFound):
		return RegisterResult{}, err
	}

	// 2. «У меня уже есть аккаунт»: пароль один раз — и привязка.
	if in.Login != "" || in.Password != "" {
		return s.linkExistingByPassword(ctx, in, data.User)
	}

	// 3. Заводить не просили — говорим прямо. Мини-апп покажет
	// развилку, а не заведёт человеку второй пустой аккаунт.
	if !in.Create {
		return RegisterResult{}, ErrTelegramUnknown
	}
	return s.registerFromTelegram(ctx, in.Bot, data.User)
}

// linkExistingByPassword — привязать телеграм к существующему аккаунту.
func (s *Service) linkExistingByPassword(
	ctx context.Context, in TelegramLogin, tgUser telegram.User,
) (RegisterResult, error) {
	login := normalizeLogin(in.Login)
	if login == "" || in.Password == "" {
		return RegisterResult{}, ErrBadCredentials
	}
	u, err := s.repo.FindByLogin(ctx, login)
	if errors.Is(err, ErrNotFound) {
		return RegisterResult{}, ErrBadCredentials
	}
	if err != nil {
		return RegisterResult{}, err
	}
	if !u.IsActive {
		return RegisterResult{}, ErrInactive
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)); err != nil {
		return RegisterResult{}, ErrBadCredentials
	}

	providerID := strconv.FormatInt(tgUser.ID, 10)
	// Занят другим — отказываем, а не переносим: слияние аккаунтов
	// делает админ руками, и делать его молча, по факту ввода пароля,
	// нельзя.
	if owner, _, err := s.repo.FindByIdentity(ctx, ProviderTelegram, providerID); err == nil {
		if owner != u.ID {
			return RegisterResult{}, telegram.ErrTaken
		}
	} else if !errors.Is(err, ErrNotFound) {
		return RegisterResult{}, err
	}

	if err := s.repo.LinkIdentity(ctx, nil, u.ID, ProviderTelegram, providerID, ""); err != nil {
		return RegisterResult{}, err
	}
	kind, err := s.upgradeKindForBot(ctx, u.ID, u.Kind, in.Bot)
	if err != nil {
		return RegisterResult{}, err
	}
	s.rememberLink(ctx, u.ID, in.Bot, tgUser)

	pair, err := s.tokens.Issue(u.ID, s.now())
	if err == nil {
		s.touchLastLogin(ctx, u.ID)
	}
	return RegisterResult{UserID: u.ID, Tokens: pair, Kind: kind}, err
}

// registerFromTelegram — новый человек: ни почты, ни пароля.
func (s *Service) registerFromTelegram(
	ctx context.Context, bot string, tgUser telegram.User,
) (RegisterResult, error) {
	// Бот определяет роль: креаторский — исполнитель, клиентский —
	// заказчик. Спрашивать это вторым экраном незачем: человек уже
	// выбрал, в какого бота написать.
	kind := KindClient
	if bot == telegram.BotCreator {
		kind = KindSpecialist
	}
	name := tgUser.DisplayName()
	providerID := strconv.FormatInt(tgUser.ID, 10)

	var userID uuid.UUID
	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		// telegram_user_id — сразу, в том же INSERT: без него у
		// человека нет ни почты, ни телефона, и CHECK
		// users_contact_present отвергнет строку. Проставить его
		// «потом» нельзя — потома не будет.
		tgID := tgUser.ID
		id, err := s.repo.CreateUser(ctx, tx, User{
			Kind: kind, DisplayName: name, TelegramUserID: &tgID,
		})
		if err != nil {
			return err
		}
		userID = id
		if err := s.repo.LinkIdentity(ctx, tx, id, ProviderTelegram, providerID, ""); err != nil {
			return err
		}
		// Контакты пишем только в пустое — а у нового человека пусто
		// всё. Юзернейм кладём как КОНТАКТ для менеджера, а не как
		// канал доставки: доставка живёт в telegram_links.
		if kind == KindSpecialist {
			link := ""
			if tgUser.Username != "" {
				link = "https://t.me/" + tgUser.Username
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, social_links)
VALUES ($1, $2, CASE WHEN $3 = '' THEN '{}'::jsonb
                     ELSE jsonb_build_object('telegram', $3::text) END)`,
				id, name, link); err != nil {
				return fmt.Errorf("insert profile: %w", err)
			}
			if err := outbox.Emit(ctx, tx, outbox.AggregateSpecialist, id.String(),
				outbox.EventSpecialistUpserted,
				map[string]string{"user_id": id.String(), "source": "telegram"}); err != nil {
				return err
			}
		} else if tgUser.Username != "" {
			if _, err := tx.Exec(ctx, `
INSERT INTO client_profiles (user_id, display_name, telegram)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
   SET display_name = COALESCE(NULLIF(client_profiles.display_name, ''), EXCLUDED.display_name),
       telegram = COALESCE(NULLIF(client_profiles.telegram, ''), EXCLUDED.telegram)`,
				id, name, "@"+tgUser.Username); err != nil {
				return fmt.Errorf("insert client profile: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return RegisterResult{}, err
	}

	s.rememberLink(ctx, userID, bot, tgUser)
	pair, err := s.tokens.Issue(userID, s.now())
	return RegisterResult{UserID: userID, Tokens: pair, IsNew: true, Kind: kind}, err
}

// upgradeKindForBot — креатор написал боту заказчиков (или наоборот).
//
// Поднимаем до `both` и НИКОГДА не опускаем: человек, который снимает
// и сам заказывает, — это один человек с двумя кабинетами, а не повод
// отобрать у него первый.
func (s *Service) upgradeKindForBot(
	ctx context.Context, userID uuid.UUID, kind, bot string,
) (string, error) {
	want := KindClient
	if bot == telegram.BotCreator {
		want = KindSpecialist
	}
	if kind == KindBoth || kind == want {
		return kind, nil
	}
	if err := s.repo.UpgradeKindToBoth(ctx, userID); err != nil {
		return kind, err
	}
	return KindBoth, nil
}

// rememberLink — записать привязку к боту, best-effort.
//
// Вход уже состоялся, и падать на записи доставки нельзя: человек
// остался бы без сессии из-за строки, которая влияет только на то,
// придёт ли ему сообщение. Но и молчать нельзя — без привязки бот
// ничего не пришлёт, и разбираться в этом будут по жалобе.
func (s *Service) rememberLink(ctx context.Context, userID uuid.UUID, bot string, tgUser telegram.User) {
	if s.tgLinks == nil {
		return
	}
	// В личке chat_id совпадает с id пользователя: мини-апп других
	// чатов и не знает, а групповые сообщения мы не шлём.
	if _, err := s.tgLinks.LinkDirect(
		ctx, userID, bot, tgUser.ID, tgUser.ID, tgUser.Username, s.now(),
	); err != nil {
		slog.Warn("auth: telegram link not saved",
			"user_id", userID.String(), "bot", bot, "err", err)
	}
}
