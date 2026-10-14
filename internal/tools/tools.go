package tools

// Tool is the interface implemented by every built-in tool.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns a JSON Schema object for the tool's arguments.
	Parameters() map[string]any
	// Run executes the tool and returns its textual result.
	Run(ctx *Context) (string, error)
}
