package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

// snapshotProjection retains immutable history/catalog segments. Ordinary
// stream and state publication only rebuilds the bounded live state window.
// Access is serialized by mu; returned views are never modified in place.
type snapshotProjection struct {
	first                                                         *messages.Message
	history                                                       []protocol.MessageView
	catalog                                                       protocol.CatalogSnapshot
	catalogVersion, settingsVersion, toolsVersion, bindingVersion uint64
	catalogReady                                                  bool
}

func messageView(m messages.Message) protocol.MessageView {
	v := protocol.MessageView{InternalSource: m.InternalSource, ID: m.ID, Role: string(m.Role), Content: m.Content, ReasoningContent: m.ReasoningContent}
	for _, call := range m.ToolCalls {
		v.ToolCallIDs = append(v.ToolCallIDs, protocol.CallID(call.ID))
		v.ToolCalls = append(v.ToolCalls, protocol.MessageViewToolCall{ID: call.ID, Name: call.Name})
	}
	return v
}

func (a *Agent) refreshSnapshotLocked() {
	if a.owner == nil {
		return
	}
	cache := &a.snapshotProjection
	var first *messages.Message
	if len(a.historyLocked()) > 0 {
		first = &a.historyLocked()[0]
	}
	if first != cache.first || len(a.historyLocked()) < len(cache.history) {
		cache.first = first
		cache.history = nil
	}
	for i := len(cache.history); i < len(a.historyLocked()); i++ {
		cache.history = append(cache.history, messageView(a.historyLocked()[i]))
	}
	a.refreshCatalogLocked()
	view := protocol.SessionView{History: cache.history, Catalog: cache.catalog}
	view.ApplyState(a.stateLocked())
	a.publishedSnapshot.Store(&view)
}

func (a *Agent) refreshSnapshotOwned() {
	a.installPersistenceFailureOwned()
	a.mu.Lock()
	a.refreshSnapshotLocked()
	a.mu.Unlock()
}

// Publication precedes acknowledgment: a caller reading Snapshot immediately
// after a successful command/completion sees that transition or a newer one.
func replyOwned[T any](a *Agent, reply chan T, value T) {
	a.refreshSnapshotOwned()
	reply <- value
}

// Live previews may advance between owner publications. They replace only the
// transcript of the last confirmed view, never read partially installed
// business state or attach a newer journal cursor to older usage/settings.
func (a *Agent) publishTranscriptPreviewLocked() {
	transcript, more := a.transcript.Snapshot()
	for {
		prior := a.publishedSnapshot.Load()
		if prior == nil {
			return
		}
		next := *prior
		next.Transcript, next.TranscriptMore = transcript, more
		if a.publishedSnapshot.CompareAndSwap(prior, &next) {
			return
		}
	}
}

func (a *Agent) refreshCatalogLocked() {
	cache := &a.snapshotProjection
	version := a.services.registry.Version()
	if !cache.catalogReady || cache.catalogVersion != a.sequences.catalogVersion || cache.settingsVersion != a.sequences.settingsRev || cache.toolsVersion != version || cache.bindingVersion != a.modelsState.activeBinding.version {
		cache.catalog = a.catalogSnapshotLocked()
		cache.catalogVersion, cache.settingsVersion, cache.toolsVersion, cache.bindingVersion = a.sequences.catalogVersion, a.sequences.settingsRev, version, a.modelsState.activeBinding.version
		cache.catalogReady = true
	}
}
