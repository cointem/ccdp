package agent

import (
	"encoding/json"
	"slices"
	"strings"
)

// Output validation retains the full schema in the registry; model-facing
// descriptions need only field names and types for accessing result.data.
func outputShape(schema map[string]any) string {
	kind, _ := schema["type"].(string)
	switch kind {
	case "object":
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			break
		}
		fields := make([]string, 0, len(properties))
		for name, value := range properties {
			child, ok := value.(map[string]any)
			if !ok {
				return encodedShape(schema)
			}
			fields = append(fields, name+":"+outputShape(child))
		}
		slices.Sort(fields)
		return "{" + strings.Join(fields, ",") + "}"
	case "array":
		if item, ok := schema["items"].(map[string]any); ok {
			return "[" + outputShape(item) + "]"
		}
	case "string", "integer", "number", "boolean", "null":
		return kind
	}
	return encodedShape(schema)
}

func encodedShape(schema map[string]any) string {
	data, _ := json.Marshal(schema)
	return string(data)
}
