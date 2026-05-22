// Package mailer — отправка транзакционных email.
//
// Sender — узкий интерфейс, чтобы воркер не зависел от конкретного провайдера
// и в тестах можно было подменить на in-memory mock. На сегодня единственная
// реализация — UnisenderGo (рекомендован для РФ-аудитории: лучше доставка в
// Mail.ru/Yandex/Sber). При отсутствии API-ключа Sender может быть nil, тогда
// воркер просто логирует пропуск (как с LLM).
package mailer

import "context"

type Message struct {
	To       string // получатель (валидный email)
	ToName   string // имя получателя (опционально, для шапки)
	Subject  string
	Plain    string // обязательное plain-text тело (для клиентов без HTML)
	HTML     string // опционально; если пусто — отдаём только plain
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}
