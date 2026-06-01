package projects

import "encoding/json"

// jsonMarshal — обёртка над json.Marshal, чтобы repo.go не тянуть encoding/json
// напрямую (читаемость), и чтобы единственная точка проверки ошибки.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
