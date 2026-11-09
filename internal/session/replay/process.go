package replay

import (
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"encoding/json"
	"fmt"
)

type Process struct {
	State     protocol.ProcessState
	Result    *session.BlobRef
	Projected bool
}

func (p *State) applyProcess(event session.Event) error {
	switch e := event.(type) {
	case *session.ProcessRegistered:
		if _, ok := p.Processes[e.State.ID]; ok {
			return fmt.Errorf("process already registered: %s", e.State.ID)
		}
		p.Processes[e.State.ID] = Process{State: *protocol.CloneProcessState(&e.State)}
		p.ProcessOrder = append(p.ProcessOrder, e.State.ID)
	case *session.ProcessStarted:
		v, ok := p.Processes[e.State.ID]
		if !ok || v.State.Terminal() {
			return fmt.Errorf("invalid process start: %s", e.State.ID)
		}
		v.State = *protocol.CloneProcessState(&e.State)
		p.Processes[e.State.ID] = v
	case *session.ProcessFinished:
		v, ok := p.Processes[e.State.ID]
		if !ok || v.Result != nil {
			return fmt.Errorf("invalid process finish: %s", e.State.ID)
		}
		v.State = *protocol.CloneProcessState(&e.State)
		v.Result = cloneRef(&e.Result)
		p.Processes[e.State.ID] = v
		p.pendingProcesses = append(p.pendingProcesses, e.State.ID)
	case *session.ProcessReleased:
		v, ok := p.Processes[e.ID]
		if !ok {
			return fmt.Errorf("unknown process release: %s", e.ID)
		}
		v.State.Released = true
		v.State.Log = ""
		v.State.Revision = e.Revision
		p.Processes[e.ID] = v
	case *session.ProcessResultsProjected:
		for i, id := range e.IDs {
			v, ok := p.Processes[id]
			if !ok || v.Result == nil || v.Projected {
				return fmt.Errorf("invalid process delivery: %s", id)
			}
			v.Projected = true
			p.Processes[id] = v
			if len(e.Outputs) > 0 {
				output := e.Outputs[i]
				var text string
				// Use the text committed at projection time, never re-render
				// conversation history when presentation code changes.
				if len(e.Texts) > 0 {
					text = e.Texts[i]
				} else {
					data, _ := json.Marshal(output)
					text = protocol.ProcessCompletionLabel + string(data)
				}
				message := messages.Message{ID: output.ResultID, Role: messages.RoleUser, InternalSource: "process_completion", Content: text, OutputRef: &messages.OutputReference{Hash: v.Result.Hash, Size: v.Result.Size, MediaType: v.Result.MediaType}}
				p.History = append(p.History, message)
				p.historyStep = append(p.historyStep, "")
			}
		}
		kept := p.pendingProcesses[:0]
		for _, id := range p.pendingProcesses {
			if !p.Processes[id].Projected {
				kept = append(kept, id)
			}
		}
		clear(p.pendingProcesses[len(kept):])
		p.pendingProcesses = kept
	}
	return nil
}

// PendingProcessResults copies only undelivered completions. Its empty path
// neither scans old processes nor clones the conversation and tool journal.
func (p *State) PendingProcessResults() []protocol.ProcessOutput {
	if p == nil || len(p.pendingProcesses) == 0 {
		return nil
	}
	out := make([]protocol.ProcessOutput, 0, len(p.pendingProcesses))
	for _, id := range p.pendingProcesses {
		v := p.Processes[id]
		out = append(out, *protocol.CloneProcessOutput(&v.State.ProcessOutput))
	}
	return out
}

func (p *State) processOutputReferences(ids []string) []messages.OutputReference {
	var refs []messages.OutputReference
	for _, id := range ids {
		if proc, ok := p.Processes[id]; ok && proc.Result != nil {
			refs = append(refs, messages.OutputReference{Hash: proc.Result.Hash, Size: proc.Result.Size, MediaType: proc.Result.MediaType})
		}
	}
	return refs
}
