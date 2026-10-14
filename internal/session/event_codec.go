package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

type wireEvent struct {
	Type    EventType       `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func encodeEvent(event Event) (wireEvent, error) {
	if event == nil {
		return wireEvent{}, errors.New("nil event")
	}
	value := reflect.ValueOf(event)
	if value.Kind() == reflect.Ptr && value.IsNil() {
		return wireEvent{}, fmt.Errorf("nil event type %T", event)
	}
	if err := event.validate(); err != nil {
		return wireEvent{}, fmt.Errorf("%s: %w", event.Type(), err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return wireEvent{}, fmt.Errorf("encode %s: %w", event.Type(), err)
	}
	return wireEvent{Type: event.Type(), Payload: data}, nil
}

func decodeEvent(w wireEvent) (Event, error) {
	if w.Type == "" || len(w.Payload) == 0 {
		return nil, errors.New("event type and payload are required")
	}
	var event Event
	switch w.Type {
	case EventTypeDeliveryRecorded:
		event = &DeliveryRecorded{}
	case EventTypeRestoreRecorded:
		event = &RestoreRecorded{}
	case EventTypeRecoveryPointRecorded:
		event = &RecoveryPointRecorded{}
	case EventTypeReviewRecorded:
		event = &ReviewRecorded{}
	case EventTypeFileMutationRecorded:
		event = &FileMutationRecorded{}
	case EventTypeChildRunRecorded:
		event = &ChildRunRecorded{}
	case EventTypeSessionCreated:
		event = &SessionCreated{}
	case EventTypeSessionImported:
		event = &SessionImported{}
	case EventTypeSessionClosed:
		event = &SessionClosed{}
	case EventTypeInputQueued:
		event = &InputQueued{}
	case EventTypeInputDelivered:
		event = &InputDelivered{}
	case EventTypeInputCancelled:
		event = &InputCancelled{}
	case EventTypeCommandScheduled:
		event = &CommandScheduled{}
	case EventTypeCommandCompleted:
		event = &CommandCompleted{}
	case EventTypeSettingsChanged:
		event = &SettingsChanged{}
	case EventTypeGoalChanged:
		event = &GoalChanged{}
	case EventTypeWorkflowChanged:
		event = &WorkflowChanged{}
	case EventTypeTurnStarted:
		event = &TurnStarted{}
	case EventTypeTurnFinished:
		event = &TurnFinished{}
	case EventTypeRequestPrepared:
		event = &RequestPrepared{}
	case EventTypeAttemptFinished:
		event = &AttemptFinished{}
	case EventTypeAssistantCommitted:
		event = &AssistantCommitted{}
	case EventTypeToolStarted:
		event = &ToolStarted{}
	case EventTypeToolFinished:
		event = &ToolFinished{}
	case EventTypeToolResultsProjected:
		event = &ToolResultsProjected{}
	case EventTypeApprovalRequested:
		event = &ApprovalRequested{}
	case EventTypeApprovalResolved:
		event = &ApprovalResolved{}
	case EventTypeHookStarted:
		event = &HookStarted{}
	case EventTypeHookFinished:
		event = &HookFinished{}
	case EventTypeContextCompacted:
		event = &ContextCompacted{}
	case EventTypeConversationReset:
		event = &ConversationReset{}
	case EventTypeConversationRewound:
		event = &ConversationRewound{}
	case EventTypeTasksChanged:
		event = &TasksChanged{}
	case EventTypeMemoryChanged:
		event = &MemoryChanged{}
	case EventTypeToolsDiscovered:
		event = &ToolsDiscovered{}
	case EventTypeUsageChanged:
		event = &UsageChanged{}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownEventType, w.Type)
	}
	if err := decodeStrict(w.Payload, event); err != nil {
		return nil, fmt.Errorf("decode %s: %w", w.Type, err)
	}
	if err := event.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", w.Type, err)
	}
	return event, nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func normalizeEvent(event Event) (Event, wireEvent, error) {
	w, err := encodeEvent(event)
	if err != nil {
		return nil, wireEvent{}, err
	}
	decoded, err := decodeEvent(w)
	if err != nil {
		return nil, wireEvent{}, err
	}
	return decoded, w, nil
}

// CloneEvent freezes a validated fact through the same wire codec as Store.
func CloneEvent(event Event) (Event, error) {
	wire, err := encodeEvent(event)
	if err != nil {
		return nil, err
	}
	return decodeEvent(wire)
}
