package web

import (
	"encoding/json"
)

func controlCodeInt64FromMessage(raw any) int64 {
	switch typed := raw.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}
