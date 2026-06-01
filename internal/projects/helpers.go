package projects

import (
	"encoding/binary"
	"encoding/json"

	"github.com/google/uuid"
)

// int64FromUUID — ключ для pg_try_advisory_xact_lock. Берём 8 байт UUID,
// читаем как big-endian int64. Коллизий мало (~2^64), цена коллизии —
// лишний skip другого проекта откатится транзакцией.
func int64FromUUID(u uuid.UUID) int64 {
	b := u[:]
	return int64(binary.BigEndian.Uint64(b[:8])) //nolint:gosec
}

// jsonMarshal — обёртка над json.Marshal, чтобы repo.go не тянуть encoding/json
// напрямую (читаемость), и чтобы единственная точка проверки ошибки.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// mustJSON — payload-ов в project_step_events. UUID/строки маршалятся
// всегда, паника защищает от тихой потери. Заменяет небезопасный
// fmt.Sprintf("{\"..\":\"..\"}") который ломается на спецсимволах в JSON.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("project event payload marshal: " + err.Error())
	}
	return b
}
