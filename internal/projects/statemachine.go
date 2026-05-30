package projects

// IsValidStepTransition — чистая функция-граф разрешённых переходов
// step_status. Не делает запросы в БД; проверяет только то, что
// «синтаксически» можно перевести шаг из from в to.
//
// Граф (бриф §4.1):
//
//	pending → in_progress
//	in_progress → done | waiting_client | skipped
//	waiting_client → done | rejected | skipped
//	rejected → in_progress
//	done | skipped → (терминальные, никаких переходов)
//
// Доп. правило: skipped разрешён только из «не-терминальных» состояний.
//
// Параметр owner здесь не используется — он валидируется на уровне
// service (например, owner=client значит что approve/request_revision
// делает клиент, не админ; но переход done сам по себе валиден). Если
// потребуется owner-зависимые правила (например, запретить специалисту
// двигать client-шаги), добавим без ломки сигнатуры.
func IsValidStepTransition(from, to StepStatus) bool {
	switch from {
	case StepPending:
		return to == StepInProgress
	case StepInProgress:
		return to == StepDone || to == StepWaitingClient || to == StepSkipped
	case StepWaitingClient:
		return to == StepDone || to == StepRejected || to == StepSkipped
	case StepRejected:
		return to == StepInProgress
	case StepDone, StepSkipped:
		// Терминалы — никуда.
		return false
	default:
		return false
	}
}

// IsTerminalStep — финальный статус шага (не подлежит автоматическому
// переоткрытию). Используется при расчёте «проект завершён».
func IsTerminalStep(s StepStatus) bool {
	return s == StepDone || s == StepSkipped
}

// IsValidProjectStatusUpdate — разрешённые ручные изменения статуса
// проекта менеджером через PATCH /admin/projects/{id}.
// Не покрывает auto-переходы (active→done, active→dispute), которые
// делает service автоматически.
//
// Граф:
//
//	draft     → active | cancelled
//	active    → on_hold | cancelled
//	on_hold   → active | cancelled
//	dispute   → active | cancelled            (resolution менеджером)
//	done      → (терминал)
//	cancelled → (терминал)
func IsValidProjectStatusUpdate(from, to ProjectStatus) bool {
	if from == to {
		return true // идемпотентность — «оставить как есть»
	}
	switch from {
	case StatusDraft:
		return to == StatusActive || to == StatusCancelled
	case StatusActive:
		return to == StatusOnHold || to == StatusCancelled
	case StatusOnHold:
		return to == StatusActive || to == StatusCancelled
	case StatusDispute:
		return to == StatusActive || to == StatusCancelled
	case StatusDone, StatusCancelled:
		return false
	default:
		return false
	}
}
