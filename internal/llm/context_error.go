package llm

import (
	"encoding/json"
	"fmt"
)

// Only explicit provider error codes permit context recovery. Ordinary
// invalid_request errors and human-readable message text are not sufficient.
func contextErrorCode(code string) bool {
	switch code {
	case "context_length_exceeded", "context_window_exceeded", "max_context_length_exceeded":
		return true
	default:
		return false
	}
}

func providerErrorCause(raw []byte) error {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil &&
		(contextErrorCode(envelope.Error.Code) || contextErrorCode(envelope.Error.Type)) {
		return ErrContextWindowExceeded
	}
	return nil
}

func structuredProviderError(message, kind, code string) error {
	if contextErrorCode(code) || contextErrorCode(kind) {
		return fmt.Errorf("llm: provider error: %s: %w", message, ErrContextWindowExceeded)
	}
	return fmt.Errorf("llm: provider error: %s", message)
}
