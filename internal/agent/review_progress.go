package agent

import "ccdp/internal/protocol"

// Review observations are UI transcript items, not messages in the parent's
// model context. The snapshot projection retains them across watch reconnects.
func (a *Agent) publishReviewItem(id protocol.CommandID, item protocol.TranscriptItem) {
	switch item.Kind {
	case "thinking", "tool", "system":
	default:
		// The child prompt and raw JSON answer are internal report machinery.
		return
	}
	if item.ID == "" {
		return
	}
	prefix := "review:" + string(id) + ":"
	item.ID = prefix + item.ID
	if item.PreviousID != "" {
		item.PreviousID = prefix + item.PreviousID
	}
	item.TurnID = ""
	if item.CallID != "" {
		item.CallID = protocol.CallID(prefix + string(item.CallID))
	}
	item.Args = append([]byte(nil), item.Args...)
	var clipped bool
	item.Text, clipped = clipTranscript(item.Text)
	item.Truncated = item.Truncated || clipped
	if len(item.Args) > transcriptArgsLimit {
		item.Args = nil
		item.Truncated = true
	}
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	a.flushStreamsLocked()
	a.transcript.mu.Lock()
	a.transcript.put(item)
	a.transcript.mu.Unlock()
	a.publishViewLocked(protocol.EventView{Kind: protocol.EventOperationProgress, SessionID: protocol.SessionID(a.SessionID()), Transcript: &item})
}
