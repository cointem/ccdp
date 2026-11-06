package sessionview

import (
	"time"

	"ccdp/internal/protocol"
)

// StreamQueue coalesces previews without owning a goroutine or timer. The
// session owner schedules the returned delay and drains at durable boundaries.
type StreamQueue struct {
	pending   map[streamKey]protocol.EventView
	order     []streamKey
	lastFlush time.Time
	lost      bool
}

type streamKey struct {
	kind    protocol.EventKind
	turn    protocol.TurnID
	step    protocol.StepID
	call    protocol.CallID
	message string
}

func (q *StreamQueue) Add(view protocol.EventView, now time.Time) time.Duration {
	if q.pending == nil {
		q.pending = make(map[streamKey]protocol.EventView)
	}
	key := streamKey{kind: view.Kind, turn: view.TurnID, step: view.StepID, call: view.CallID, message: view.MessageID}
	if view.Tool != nil {
		key.call = view.Tool.ID
	}
	prior, exists := q.pending[key]
	if !exists {
		q.order = append(q.order, key)
	} else {
		view.Text = prior.Text + view.Text
		if prior.Tool != nil && view.Tool != nil {
			tool := *view.Tool
			tool.Output = prior.Tool.Output + tool.Output
			if tool.Progress != nil {
				tool.Progress = protocol.CloneProcessProgress(tool.Progress)
				if p := prior.Tool.Progress; p != nil && p.ID == tool.Progress.ID {
					tool.Progress.Truncated = tool.Progress.Truncated || p.Truncated || p.Next != tool.Progress.Offset
					tool.Progress.Offset = p.Offset
				}
			}
			if len(tool.Output) > TextLimit {
				tool.Output, _ = Tail(tool.Output, TextLimit)
				if tool.Progress != nil {
					tool.Progress = protocol.CloneProcessProgress(tool.Progress)
					tool.Progress.Truncated = true
				}
				q.lost = true
			}
			view.Tool = &tool
		}
	}
	if len(view.Text) > TextLimit {
		view.Text, _ = Tail(view.Text, TextLimit)
		q.lost = true
	}
	if view.Tool != nil && len(view.Tool.Output) > TextLimit {
		tool := *view.Tool
		tool.Output, _ = Tail(tool.Output, TextLimit)
		if tool.Progress != nil {
			tool.Progress = protocol.CloneProcessProgress(tool.Progress)
			tool.Progress.Truncated = true
		}
		view.Tool = &tool
		q.lost = true
	}
	q.pending[key] = view
	return max(time.Duration(0), 50*time.Millisecond-now.Sub(q.lastFlush))
}
func (q *StreamQueue) Drain(now time.Time) ([]protocol.EventView, bool) {
	if len(q.order) == 0 {
		return nil, false
	}
	q.lastFlush = now
	out := make([]protocol.EventView, 0, len(q.order))
	for _, key := range q.order {
		out = append(out, q.pending[key])
	}
	q.order = nil
	q.pending = nil
	lost := q.lost
	q.lost = false
	return out, lost
}
