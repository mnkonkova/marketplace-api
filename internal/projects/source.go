package projects

import "github.com/google/uuid"

// DetectSource применяет правило «если есть lead_id → marketplace,
// иначе берём hint от менеджера». Изолирует это решение в одной точке —
// бизнес-инвариант (см. бриф §1: «Источники проектов помечаются полем
// source»), и при создании любым способом проходит через эту функцию.
//
//	leadID != nil      → ProjectSource("marketplace") вне зависимости от hint.
//	leadID == nil      → hint, если он валидный non-marketplace; иначе manual.
//	hint == marketplace без leadID запрещён (бракованный input) → manual.
//
// Возвращает выбранный source.
func DetectSource(leadID *uuid.UUID, hint ProjectSource) ProjectSource {
	if leadID != nil {
		return SourceMarketplace
	}
	switch hint {
	case SourceReferral, SourceReturningClient:
		return hint
	case SourceManual:
		return SourceManual
	default:
		// hint пустой / "marketplace" без leadID / любой неизвестный → manual.
		return SourceManual
	}
}
