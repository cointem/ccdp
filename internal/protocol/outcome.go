package protocol

// ExecutionOutcome describes business completion independently of progress,
// report persistence, or resource cleanup. UI run names are an adapter only.
type ExecutionOutcome string

const (
	OutcomeSuccess   ExecutionOutcome = "success"
	OutcomePartial   ExecutionOutcome = "partial"
	OutcomeFailed    ExecutionOutcome = "failed"
	OutcomeCancelled ExecutionOutcome = "cancelled"
)

func (o ExecutionOutcome) RunStatus() string {
	if o == OutcomeSuccess {
		return "succeeded"
	}
	return string(o)
}

func (r RunView) Outcome() ExecutionOutcome {
	switch r.Status {
	case "succeeded":
		return OutcomeSuccess
	case "partial":
		return OutcomePartial
	case "cancelled", "interrupted":
		return OutcomeCancelled
	default:
		return OutcomeFailed
	}
}
