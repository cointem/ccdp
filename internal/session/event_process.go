package session

import (
	"ccdp/internal/protocol"
	"fmt"
)

const (
	EventTypeProcessRegistered       EventType = "ProcessRegistered"
	EventTypeProcessStarted          EventType = "ProcessStarted"
	EventTypeProcessFinished         EventType = "ProcessFinished"
	EventTypeProcessReleased         EventType = "ProcessReleased"
	EventTypeProcessResultsProjected EventType = "ProcessResultsProjected"
)

type ProcessRegistered struct {
	State protocol.ProcessState `json:"state"`
}
type ProcessStarted struct {
	State protocol.ProcessState `json:"state"`
}
type ProcessFinished struct {
	State  protocol.ProcessState `json:"state"`
	Result BlobRef               `json:"result"`
}
type ProcessReleased struct {
	ID       string `json:"process_id"`
	Revision uint64 `json:"revision"`
}

// Empty Outputs means these IDs were delivered in ToolResultsProjected in the
// same transaction. Otherwise each output becomes a runtime-data message.
type ProcessResultsProjected struct {
	TurnID    string                   `json:"turn_id,omitempty"`
	StepID    string                   `json:"step_id,omitempty"`
	ToolCalls map[string]string        `json:"tool_calls,omitempty"`
	IDs       []string                 `json:"process_ids"`
	Outputs   []protocol.ProcessOutput `json:"outputs,omitempty"`
	Budget    Budget                   `json:"budget"`
}

func (ProcessRegistered) Type() EventType       { return EventTypeProcessRegistered }
func (ProcessStarted) Type() EventType          { return EventTypeProcessStarted }
func (ProcessFinished) Type() EventType         { return EventTypeProcessFinished }
func (ProcessReleased) Type() EventType         { return EventTypeProcessReleased }
func (ProcessResultsProjected) Type() EventType { return EventTypeProcessResultsProjected }
func (ProcessRegistered) seal()                 {}
func (ProcessStarted) seal()                    {}
func (ProcessFinished) seal()                   {}
func (ProcessReleased) seal()                   {}
func (ProcessResultsProjected) seal()           {}
func validateProcess(s protocol.ProcessState) error {
	if err := requireID("process_id", s.ID); err != nil {
		return err
	}
	if err := requireID("origin_tool_id", s.Origin); err != nil {
		return err
	}
	if s.Revision == 0 {
		return fmt.Errorf("process revision is required")
	}
	return nil
}
func (e ProcessRegistered) validate() error { return validateProcess(e.State) }
func (e ProcessStarted) validate() error    { return validateProcess(e.State) }
func (e ProcessFinished) validate() error {
	if err := validateProcess(e.State); err != nil {
		return err
	}
	if !e.State.Terminal() || e.State.ResultID == "" || e.State.OutputID != e.Result.Hash {
		return fmt.Errorf("terminal process result reference is required")
	}
	return e.Result.validate()
}
func (e ProcessReleased) validate() error { return requireID("process_id", e.ID) }
func (e ProcessResultsProjected) validate() error {
	if err := e.Budget.validate(); err != nil {
		return err
	}
	if len(e.IDs) == 0 {
		return fmt.Errorf("process projection is empty")
	}
	if len(e.Outputs) != 0 && len(e.Outputs) != len(e.IDs) {
		return fmt.Errorf("process projection count mismatch")
	}
	for i, id := range e.IDs {
		if err := requireID("process_id", id); err != nil {
			return err
		}
		if len(e.Outputs) > 0 && (e.Outputs[i].ID != id || e.Outputs[i].ResultID == "" || e.Outputs[i].OutputID == "" || !e.Outputs[i].Terminal()) {
			return fmt.Errorf("invalid process projection")
		}
	}
	return nil
}
