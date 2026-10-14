package sessionview

import (
	"ccdp/internal/protocol"
)

// Hub owns subscription channels and stream ordering. Its owner serializes
// Subscribe, Publish, Remove and Close; the hub never calls back into the owner
// while publishing. unsubscribe routes consumer Close back to that owner.
type Hub struct {
	epoch       string
	sequence    uint64
	next        uint64
	closed      bool
	subscribers map[uint64]*Subscription
	unsubscribe func(uint64)
}

func NewHub(epoch string, unsubscribe func(uint64)) *Hub {
	return &Hub{epoch: epoch, subscribers: make(map[uint64]*Subscription), unsubscribe: unsubscribe}
}

func (h *Hub) Cursor(rev protocol.Revision) protocol.Cursor {
	return protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration, StreamEpoch: h.epoch, EventSeq: h.sequence}
}
func (h *Hub) Stamp(update *protocol.Update) {
	h.sequence++
	update.Cursor.StreamEpoch = h.epoch
	update.Cursor.EventSeq = h.sequence
}

func (h *Hub) Subscribe(initial protocol.Update) *Subscription {
	h.next++
	s := &Subscription{id: h.next, ch: make(chan protocol.Update, 64), done: make(chan struct{}), unsubscribe: h.unsubscribe}
	if h.closed {
		close(s.done)
		close(s.ch)
		return s
	}
	h.subscribers[s.id] = s
	h.send(s, initial)
	return s
}

// Publish reserves a slot for the terminal resync marker. A slow consumer
// cannot silently lose its recovery signal or receive later orphan deltas.
func (h *Hub) Publish(updates ...protocol.Update) {
	for _, s := range h.subscribers {
		for _, u := range updates {
			h.send(s, u)
		}
	}
}
func (h *Hub) send(s *Subscription, u protocol.Update) {
	select {
	case <-s.done:
		return
	default:
	}
	if len(s.ch) < cap(s.ch)-1 {
		s.ch <- protocol.CloneUpdate(u)
		return
	}
	s.ch <- protocol.Update{Type: protocol.UpdateResyncRequired, Cursor: u.Cursor, Revision: u.Revision}
	h.Remove(s.id)
}
func (h *Hub) Remove(id uint64) {
	s := h.subscribers[id]
	if s == nil {
		return
	}
	delete(h.subscribers, id)
	close(s.done)
	close(s.ch)
}
func (h *Hub) Close() {
	h.closed = true
	for id := range h.subscribers {
		h.Remove(id)
	}
}

type Subscription struct {
	id          uint64
	ch          chan protocol.Update
	done        chan struct{}
	unsubscribe func(uint64)
}

func (s *Subscription) Updates() <-chan protocol.Update { return s.ch }
func (s *Subscription) Done() <-chan struct{}           { return s.done }
func (s *Subscription) Close() error                    { s.unsubscribe(s.id); return nil }

var _ protocol.Subscription = (*Subscription)(nil)

// Resync ends every current observation after a bounded producer inbox loses
// intermediate updates. Later subscribers start from a fresh owner snapshot.
func (h *Hub) Resync(update protocol.Update) {
	for id, subscription := range h.subscribers {
		h.send(subscription, update)
		h.Remove(id)
	}
}
