package tools

import (
	"encoding/json"

	"ccdp/internal/protocol"
)

// Tool is the interface implemented by every built-in tool.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns a JSON Schema object for the tool's arguments.
	Parameters() map[string]any
	// Execute performs exactly one operation. args have already passed the
	// frozen definition's default preparation and validation.
	Execute(ctx *Context, args map[string]any) (Result, error)
}

// Result keeps execution output separate from its presentation and validation.
// An invalid Data value does not undo an otherwise successful operation.
type Result struct {
	Text          string
	Data          json.RawMessage
	DataTruncated bool
	DataError     string
	Code          *protocol.CodeResult
}

// OutputDefiner declares the JSON schema of Result.Data, when present.
type OutputDefiner interface {
	OutputSchema() map[string]any
}

// InputDefiner supplies the original input schema for external tools whose
// advertised parameters also contain harness-owned authorization metadata.
type InputDefiner interface {
	InputSchema() map[string]any
}

// ProgramTool opts an implementation into RunCode. The registry freezes this
// host capability; a name or an external JSON schema cannot grant it.
type ProgramTool interface {
	ProgramCallable() bool
}
