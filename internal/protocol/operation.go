package protocol

import "context"

type OperationResult struct {
	ID       CommandID `json:"id"`
	Complete bool      `json:"complete"`
	Status   string    `json:"status"`
	Output   string    `json:"output"`
	Error    string    `json:"error,omitempty"`
	Code     ErrorCode `json:"code,omitempty"`
}

// InputResult distinguishes delivery from the outcome of the consuming turn.
// A queued cancellation has no TurnID; a delivered input can still be running.
type InputResult struct {
	ID        InputID           `json:"id"`
	Delivered bool              `json:"delivered"`
	TurnID    TurnID            `json:"turn_id,omitempty"`
	Status    TurnOutcomeStatus `json:"status,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// OperationReader recovers missed completions after observation reconnects.
// It is optional for SessionClient adapters and never resubmits work.
type OperationReader interface {
	ReadOperation(context.Context, CommandID) (OperationResult, error)
	ReadInput(context.Context, InputID) (InputResult, error)
}
