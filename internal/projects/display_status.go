package projects

// display_status — чистые функции для расчёта UI-статусов.
// Источник истины для лейблов «В работе» / «Ждёт вас» / «Готово» / ...
// Один и тот же набор правил используется и в обычной выдаче клиенту,
// и в admin-сводке. См. docs/CRM_V5_BRIEF.md §4.6.
//
// Правила построены так, чтобы фронт ничего не считал — только рендерил.
// Каждое из трёх derive* можно тестировать в изоляции.

// DeriveProjectDisplayStatus считает «человеческий» статус всего проекта.
// Приоритет:
//  1. on_hold / cancelled → как есть (терминальные/паузные)
//  2. dispute → on_hold (для клиента это пауза в работе)
//  3. status=done → completed
//  4. есть waiting_client+owner=client → waiting_action (мяч у клиента)
//  5. есть любой активный шаг (in_progress | waiting_client) → in_progress
//  6. иначе → not_started
//
// hasActiveWork включает waiting_client независимо от owner: по стейт-машине
// в waiting_client попадают только из in_progress, значит работа уже шла.
// Это закрывает кейс «waiting_client+team» (например, ждём отчёт студии) —
// для клиента это «В работе», а не «Впереди».
func DeriveProjectDisplayStatus(status ProjectStatus, steps []StepView) ProjectDisplayStatus {
	switch status {
	case ProjectStatusOnHold:
		return ProjectDisplayOnHold
	case ProjectStatusCancelled:
		return ProjectDisplayCancelled
	case ProjectStatusDispute:
		return ProjectDisplayOnHold
	case ProjectStatusDone:
		return ProjectDisplayCompleted
	}

	hasWaitingClient := false
	hasActiveWork := false
	for _, st := range steps {
		switch st.Status {
		case StepStatusWaitingClient:
			hasActiveWork = true
			if st.Owner == OwnerClient {
				hasWaitingClient = true
			}
		case StepStatusInProgress:
			hasActiveWork = true
		}
	}
	if hasWaitingClient {
		return ProjectDisplayWaitingAction
	}
	if hasActiveWork {
		return ProjectDisplayInProgress
	}
	return ProjectDisplayNotStarted
}

// DeriveStageDisplayStatus считает статус одной стадии + done/total.
// completed = все шаги в (done|skipped); active = есть хоть один не-pending;
// not_started = все pending.
func DeriveStageDisplayStatus(steps []StepView) (StageDisplayStatus, int, int) {
	if len(steps) == 0 {
		return StageDisplayNotStarted, 0, 0
	}
	total := len(steps)
	done := 0
	hasActive := false
	for _, st := range steps {
		switch st.Status {
		case StepStatusDone, StepStatusSkipped:
			done++
		case StepStatusPending:
			// nothing
		default:
			hasActive = true
		}
	}
	if done == total {
		return StageDisplayCompleted, done, total
	}
	if hasActive || done > 0 {
		return StageDisplayActive, done, total
	}
	return StageDisplayNotStarted, done, total
}

// DeriveCurrentStep выбирает «текущий» шаг для hero-блока кабинета клиента.
// Приоритет:
//  1. waiting_client + owner=client (клиент должен действовать)
//  2. waiting_client + owner=team (ждём что-то от студии — но клиент видит)
//  3. in_progress
//  4. первый pending
// Возвращает указатель на запись из steps (не аллоцирует копию) — если
// каллер использует view, он же владеет памятью.
func DeriveCurrentStep(steps []StepView) *StepView {
	var firstPending *StepView
	var firstInProgress *StepView
	var firstWaitingClientOwnerClient *StepView
	var firstWaitingClientOwnerTeam *StepView

	for i := range steps {
		s := &steps[i]
		switch s.Status {
		case StepStatusWaitingClient:
			if s.Owner == OwnerClient {
				if firstWaitingClientOwnerClient == nil {
					firstWaitingClientOwnerClient = s
				}
			} else if firstWaitingClientOwnerTeam == nil {
				firstWaitingClientOwnerTeam = s
			}
		case StepStatusInProgress:
			if firstInProgress == nil {
				firstInProgress = s
			}
		case StepStatusPending:
			if firstPending == nil {
				firstPending = s
			}
		}
	}

	switch {
	case firstWaitingClientOwnerClient != nil:
		return firstWaitingClientOwnerClient
	case firstWaitingClientOwnerTeam != nil:
		return firstWaitingClientOwnerTeam
	case firstInProgress != nil:
		return firstInProgress
	case firstPending != nil:
		return firstPending
	}
	return nil
}

// DeriveProgress — взвешенный процент выполнения по шагам. Считается по
// видимым клиенту шагам (steps на входе — уже видимые). done+skipped дают
// полный вес; in_progress / waiting_client — половину; остальные — 0.
// Возвращает 0..100 в float (фронт сам округляет).
func DeriveProgress(steps []StepView) float64 {
	if len(steps) == 0 {
		return 0
	}
	var total, accum float64
	for _, st := range steps {
		w := float64(st.Weight)
		if w <= 0 {
			w = 1
		}
		total += w
		switch st.Status {
		case StepStatusDone, StepStatusSkipped:
			accum += w
		case StepStatusInProgress, StepStatusWaitingClient:
			accum += w / 2
		}
	}
	if total == 0 {
		return 0
	}
	return (accum / total) * 100
}
