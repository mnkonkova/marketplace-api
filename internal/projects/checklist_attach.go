package projects

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// ChecklistAttacher — подключает проекту действующий чек-лист из
// библиотеки. Реализуется publications.Service.AttachActiveChecklist.
//
// Через интерфейс, а не прямым вызовом: projects и publications друг о
// друге не знают, и заводить между ними связь ради одной операции
// значило бы склеить два домена навсегда. Тот же приём уже применён к
// воронке по умолчанию — см. DefaultPipelineProvider.
type ChecklistAttacher interface {
	// AttachActiveChecklist возвращает, сколько пунктов скопировано.
	// Ноль — штатный ответ: библиотека пуста или действующих шаблонов
	// несколько, и выбирать за человека нельзя.
	AttachActiveChecklist(ctx context.Context, projectID, actor uuid.UUID) (int, error)
}

// WithChecklistAttacher — подключает библиотеку чек-листов. Без него
// проекты создаются без чек-листа, как было раньше.
func (s *Service) WithChecklistAttacher(a ChecklistAttacher) *Service {
	s.checklistAttacher = a
	return s
}

// attachChecklist — подключить чек-лист новому проекту с креаторами.
//
// Зовётся ПОСЛЕ создания и НЕ внутри его транзакции, и это осознанно:
// проект без чек-листа — рабочее состояние (менеджер подключит руками),
// а вот проект, не созданный из-за пустой библиотеки, — поломка на
// ровном месте. Поэтому ошибка здесь только логируется.
//
// Только creators_turnkey: чек-лист описывает требования к выкладке, а
// у продакшна и общих проектов выкладок нет вовсе.
func (s *Service) attachChecklist(ctx context.Context, projectID, actor uuid.UUID, kind ProjectKind) {
	if s.checklistAttacher == nil || kind != KindCreatorsTurnkey {
		return
	}
	n, err := s.checklistAttacher.AttachActiveChecklist(ctx, projectID, actor)
	if err != nil {
		slog.Error("чек-лист не подключился к новому проекту",
			"project_id", projectID, "err", err)
		return
	}
	if n == 0 {
		// Не ошибка, но и не молчание: без этой записи «чек-листа нет»
		// выглядит как поломка, хотя причина — в библиотеке.
		slog.Info("чек-лист новому проекту не подключён: нет единственного действующего шаблона",
			"project_id", projectID)
		return
	}
	slog.Info("чек-лист подключён новому проекту", "project_id", projectID, "items", n)
}
