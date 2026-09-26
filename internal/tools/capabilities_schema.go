package tools

// WithCapabilityRequest adds the harness-owned authorization field to an
// external tool schema without mutating the extension's schema value.
func WithCapabilityRequest(schema map[string]any) map[string]any {
	copy := make(map[string]any, len(schema))
	for key, value := range schema {
		copy[key] = value
	}
	properties := map[string]any{}
	if source, ok := schema["properties"].(map[string]any); ok {
		for key, value := range source {
			properties[key] = value
		}
	}
	properties["requested_capabilities"] = capabilityRequestSchema()
	copy["properties"] = properties
	return copy
}

func capabilityRequestSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":      map[string]any{"type": "string", "enum": []string{"directory", "network", "local_service"}},
				"path":      map[string]any{"type": "string", "description": "Existing absolute directory path for a directory request."},
				"access":    map[string]any{"type": "string", "enum": []string{"write", "outbound"}},
				"direction": map[string]any{"type": "string", "enum": []string{"connect", "listen"}},
				"protocol":  map[string]any{"type": "string", "enum": []string{"tcp", "udp"}},
				"address":   map[string]any{"type": "string", "enum": []string{"localhost"}},
				"port":      map[string]any{"type": "integer", "minimum": 1, "maximum": 65535},
			},
			"required": []string{"kind"},
		},
		"description": "Extra sandbox capabilities for this invocation only: write access to a specific directory, outbound network, or a localhost service with direction, protocol and port. Host files are readable by default except explicitly denied paths. Approval scope is one call or this session. Seatbelt matches localhost plus port and may include other host-local addresses on that port.",
	}
}
