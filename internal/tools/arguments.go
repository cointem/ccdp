package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"

	"strconv"
)

// StringArg extracts a string argument with a fallback.
func StringArg(args map[string]any, key, fallback string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

// IntArg extracts an int argument with a fallback. New runtime/tool code
// should prefer IntArgChecked so malformed, fractional, or overflowing JSON
// numbers cannot silently turn into an unrelated offset, pid, or timeout.
func IntArg(args map[string]any, key string, fallback int) int {
	n, err := IntArgChecked(args, key, fallback)
	if err != nil {
		return fallback
	}
	return n
}

// IntArgChecked parses the integer forms produced by both the legacy JSON
// decoder (float64) and the strict UseNumber decoder (json.Number). It rejects
// fractional, non-finite, and out-of-range values instead of truncating them.
func IntArgChecked(args map[string]any, key string, fallback int) (int, error) {
	if v, ok := args[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return fallback, fmt.Errorf("tools: %s must be a finite integer", key)
			}
			return checkedIntegerString(key, strconv.FormatFloat(n, 'g', -1, 64), fallback)
		case float32:
			if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
				return fallback, fmt.Errorf("tools: %s must be a finite integer", key)
			}
			return checkedIntegerString(key, strconv.FormatFloat(float64(n), 'g', -1, 32), fallback)
		case int:
			return n, nil
		case int8:
			return int(n), nil
		case int16:
			return int(n), nil
		case int32:
			return int(n), nil
		case int64:
			return checkedIntegerString(key, strconv.FormatInt(n, 10), fallback)
		case uint:
			return checkedIntegerString(key, strconv.FormatUint(uint64(n), 10), fallback)
		case uint8:
			return int(n), nil
		case uint16:
			return int(n), nil
		case uint32:
			return checkedIntegerString(key, strconv.FormatUint(uint64(n), 10), fallback)
		case uint64:
			return checkedIntegerString(key, strconv.FormatUint(n, 10), fallback)
		case json.Number:
			return checkedIntegerString(key, string(n), fallback)
		default:
			return fallback, fmt.Errorf("tools: %s must be an integer", key)
		}
	}
	return fallback, nil
}

func checkedIntegerString(key, value string, fallback int) (int, error) {
	rational, ok := new(big.Rat).SetString(value)
	if !ok || !rational.IsInt() {
		return fallback, fmt.Errorf("tools: %s must be an integer", key)
	}
	n := rational.Num()
	if !n.IsInt64() {
		return fallback, fmt.Errorf("tools: %s is outside int range", key)
	}
	value64 := n.Int64()
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	if value64 < minInt || value64 > maxInt {
		return fallback, fmt.Errorf("tools: %s is outside int range", key)
	}
	return int(value64), nil
}

// BoolArg extracts a boolean argument with a fallback (bool from JSON).
func BoolArg(args map[string]any, key string, fallback bool) bool {
	if v, ok := args[key]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

// DumpArgs is a debug helper for tool authors.
func DumpArgs(args map[string]any) string {
	b, _ := json.Marshal(args)
	return string(b)
}

// WrapError formats a tool error in a stable, parseable way.
func WrapError(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

// boundedToolString enforces the output admission limit at the final tool
// boundary, including status text appended after subprocess output.
func boundedToolString(ctx *Context, value string) string {
	limit := DefaultOutputLimit
	if ctx != nil && ctx.OutputLimit > 0 {
		limit = ctx.OutputLimit
	}
	if limit <= 0 || len(value) <= limit {
		return value
	}
	marker := "\n…[tool output truncated]"
	if limit <= len(marker) {
		return truncateUTF8(value, limit)
	}
	return truncateUTF8(value, limit-len(marker)) + marker
}
