// Package events implements a lightweight domain event bus. It is the glue of
// the plugin host (the deepseek-harness "ctx.emit" idea): plugins subscribe to
// lifecycle topics such as tool execution and session state, and the agent
// emits on those topics at well-defined points. Subscribing returns a disposer
// so plugins can detach cleanly on unload.
package events

import (
	"sync"
)

// Topic names follow the "domain/action" convention used by deepseek-harness.
type Topic string

const (
	// Tool pipeline topics (emitted around every tool execution).
	TopicToolPreExecute  Topic = "tools/pre-execute"
	TopicToolPostExecute Topic = "tools/post-execute"
	TopicToolResult      Topic = "tools/result"
	TopicToolChange      Topic = "tools/change"

	// Session lifecycle topics.
	TopicSessionCreated  Topic = "session/created"
	TopicSessionResumed  Topic = "session/resumed"
	TopicSessionDisposed Topic = "session/disposed"

	// Conversation / accounting topics.
	TopicMessageAdded Topic = "session/message-added"
	TopicUsageUpdated Topic = "session/usage-updated"
	TopicCompacted    Topic = "session/compacted"
)

// Payload is arbitrary data attached to an event. Payload types are defined in
// this package so producers and consumers share a stable contract.
type Payload any

// Handler receives events of a subscribed topic.
type Handler func(topic Topic, payload Payload)

// Bus is a concurrent, synchronous, multi-subscriber event bus. Emit runs
// handlers in registration order; a panicking handler is isolated so one bad
// plugin cannot crash the agent.
type Bus struct {
	mu   sync.RWMutex
	subs map[Topic][]Handler
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[Topic][]Handler{}}
}

// Subscribe registers h for a topic and returns a disposer that detaches it.
func (b *Bus) Subscribe(topic Topic, h Handler) func() {
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], h)
	i := len(b.subs[topic]) - 1
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		slice := b.subs[topic]
		if i < len(slice) {
			b.subs[topic] = append(slice[:i], slice[i+1:]...)
		}
	}
}

// Emit synchronously delivers payload to all handlers of topic, in
// registration order. Handler panics are recovered and swallowed.
func (b *Bus) Emit(topic Topic, payload Payload) {
	b.mu.RLock()
	handlers := b.subs[topic]
	b.mu.RUnlock()
	for _, h := range handlers {
		func() {
			defer func() { _ = recover() }()
			h(topic, payload)
		}()
	}
}

// Subscribers returns the number of handlers registered for a topic.
func (b *Bus) Subscribers(topic Topic) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[topic])
}

// ---------- Payload types ----------

// ToolEvent describes one tool invocation in the pipeline.
type ToolEvent struct {
	ToolName string         `json:"tool_name"`
	Args     map[string]any `json:"args"`
	Status   string         `json:"status"` // running | success | error | denied
	Output   string         `json:"output,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// MessageEvent describes a message appended to the conversation.
type MessageEvent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// UsageEvent reports an update to the session's token/cost accounting.
type UsageEvent struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TurnCount    int     `json:"turn_count"`
	Cost         float64 `json:"cost"`
}

// SessionEvent reports a session lifecycle transition.
type SessionEvent struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace,omitempty"`
}
