package projects

// ProgressView — расчёт прогресса проекта по весам. Используется
// клиентским ЛК для прогресс-бара и админкой для отчётов.
//
// Подход: completed = шаги в финальных статусах (done или skipped),
// total = все шаги (включая skipped, чтобы skip'ы не сокращали
// знаменатель и не «прыгал» прогресс при skip'е опционального шага).
type ProgressView struct {
	Percent         int `json:"percent"`
	CompletedWeight int `json:"completed_weight"`
	TotalWeight     int `json:"total_weight"`
	// CurrentStepID — первый незавершённый шаг по sort_order.
	// nil если все done/skipped (проект в финале).
	CurrentStepID *string `json:"current_step_id,omitempty"`
}

// CalcProgress принимает плоский список шагов в порядке sort_order.
// Идемпотентен; не модифицирует входной слайс.
//
// Правила:
//   - completed_weight = сумма weight шагов, где status ∈ {done, skipped}.
//   - total_weight = сумма weight всех шагов.
//   - percent = round(completed_weight * 100 / total_weight), clamp 0..100.
//   - current = первый шаг по sort_order, у которого status ∉ {done, skipped}.
//
// total_weight=0 (пустой набор шагов) → percent=0, current=nil.
func CalcProgress(steps []StepView) ProgressView {
	if len(steps) == 0 {
		return ProgressView{}
	}
	var done, total int
	var current *StepView
	for i := range steps {
		s := &steps[i]
		total += s.Weight
		switch s.Status {
		case StepDone, StepSkipped:
			done += s.Weight
		default:
			if current == nil {
				current = s
			}
		}
	}
	pct := 0
	if total > 0 {
		// Округление до ближайшего целого: +total/2 в числителе.
		pct = (done*100 + total/2) / total
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
	}
	out := ProgressView{
		Percent:         pct,
		CompletedWeight: done,
		TotalWeight:     total,
	}
	if current != nil {
		id := current.ID.String()
		out.CurrentStepID = &id
	}
	return out
}
