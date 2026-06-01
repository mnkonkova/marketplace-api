package projects

// Файл содержит три ЧИСТЫХ функции, выводящих UI-статусы из сырого
// состояния проекта и его шагов. Бизнес-правила здесь — единственный
// источник истины: фронт ТОЛЬКО рендерит результат, никакой собственной
// derive-логики не имеет (см. бриф §1).

// DeriveProjectDisplayStatus возвращает UI-статус из связки
// project.status + плоский список видимых клиенту шагов.
//
// Правила:
//  1. Терминальные project.status побеждают (done → completed,
//     cancelled → cancelled, on_hold → on_hold, dispute → dispute —
//     отдельный статус, бриф §3.1 разрешает добавить).
//  2. Иначе анализ шагов:
//     - есть waiting_client + owner=client → waiting_action;
//     - есть in_progress ИЛИ есть done (любая активность началась) →
//     in_progress;
//     - иначе → not_started.
//
// waiting_action приоритетнее in_progress: если в один момент есть и
// in_progress на team-шаге, и client_review в waiting_client — клиент
// в первую очередь видит «требуется ваше действие».
func DeriveProjectDisplayStatus(projectStatus ProjectStatus, steps []StepView) ProjectDisplayStatus {
	switch projectStatus {
	case StatusDone:
		return DisplayStatusCompleted
	case StatusCancelled:
		return DisplayStatusCancelled
	case StatusOnHold:
		return DisplayStatusOnHold
	case StatusDispute:
		return DisplayStatusDispute
	}

	// Активный (active) или draft проект — смотрим на шаги.
	// waiting_action — отдельный «жёлтый» статус только когда шаг
	// требует действия клиента (owner=client). Любой другой
	// waiting_client / in_progress / done означает что работа УЖЕ идёт
	// (по state-machine waiting_client достижим только из in_progress),
	// поэтому общий статус — in_progress.
	hasWaitingClientForClient := false
	hasActiveWork := false
	hasDone := false

	for _, s := range steps {
		switch s.Status {
		case StepWaitingClient:
			if s.Owner == OwnerClient {
				hasWaitingClientForClient = true
			}
			// Любой waiting_client — это уже признак активного проекта
			// (шаг был in_progress перед этим).
			hasActiveWork = true
		case StepInProgress:
			hasActiveWork = true
		case StepDone, StepSkipped:
			hasDone = true
		}
	}

	switch {
	case hasWaitingClientForClient:
		return DisplayStatusWaitingAction
	case hasActiveWork || hasDone:
		return DisplayStatusInProgress
	default:
		return DisplayStatusNotStarted
	}
}

// DeriveStageDisplayStatus — статус одной стадии и счётчики
// «N из M шагов завершено».
//
//	all-done или skipped     → completed
//	0 done и без active      → not_started
//	иначе                    → active
//
// Возвращает (статус, done, total).
func DeriveStageDisplayStatus(steps []StepView) (StageDisplayStatus, int, int) {
	total := len(steps)
	done := 0
	hasActive := false
	for _, s := range steps {
		if s.Status == StepDone || s.Status == StepSkipped {
			done++
		}
		if s.Status == StepInProgress || s.Status == StepWaitingClient {
			hasActive = true
		}
	}
	switch {
	case total > 0 && done == total:
		return StageCompleted, done, total
	case done == 0 && !hasActive:
		return StageNotStarted, done, total
	default:
		return StageActive, done, total
	}
}

// DeriveCurrentStep возвращает «текущий» шаг для отображения в шапке.
// Приоритет:
//  1. waiting_client + owner=client (требуется действие клиента);
//  2. любой in_progress;
//  3. waiting_client (любой owner — редкий case для system-шагов);
//  4. первый pending по sort_order.
//
// Возвращает nil только если все шаги terminal (done/skipped).
//
// На вход поступает уже отсортированный по sort_order слайс
// (так возвращает repo.ListStepsWithStage).
func DeriveCurrentStep(steps []StepView) *StepView {
	for i := range steps {
		if steps[i].Status == StepWaitingClient && steps[i].Owner == OwnerClient {
			return &steps[i]
		}
	}
	for i := range steps {
		if steps[i].Status == StepInProgress {
			return &steps[i]
		}
	}
	for i := range steps {
		if steps[i].Status == StepWaitingClient {
			return &steps[i]
		}
	}
	for i := range steps {
		if steps[i].Status == StepPending {
			return &steps[i]
		}
	}
	return nil
}
