package replay

import "ccdp/internal/session"

// OutputReference authorizes only committed tool outputs, never request or
// image blobs. Compaction preserves access to earlier results in this session.
func (p *State) OutputReference(id string) (session.BlobRef, bool) {
	ref, ok := p.outputs[id]
	return ref, ok
}

func (p *State) indexOutputs(event session.Event) {
	add := func(ref *session.BlobRef) {
		if ref != nil && ref.Hash != "" {
			if p.outputs == nil {
				p.outputs = make(map[string]session.BlobRef)
			}
			p.outputs[ref.Hash] = *ref
		}
	}
	message := func(m session.Message) {
		for _, ref := range m.SourceOutputs {
			add(&ref)
		}
		for _, block := range m.Content {
			if block.ToolResult != nil {
				add(block.ToolResult.Blob)
			}
		}
	}
	switch e := event.(type) {
	case *session.ProcessFinished:
		add(&e.Result)
	case *session.ToolFinished:
		add(e.RawOutput)
		add(e.Result.Blob)
	case *session.AssistantCommitted:
		message(e.Message)
	case *session.ConversationReset:
		for _, m := range e.Messages {
			message(m)
		}
	case *session.ContextCompacted:
		message(e.Summary)
	}
}
