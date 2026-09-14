package agent

import "ccdp/internal/session"

// restoreDiscoveredFromStore rebuilds deferred-tool discovery from typed
// ToolsDiscovered facts.  The diagnostics stream is intentionally ignored:
// text matching a trace line cannot prove that a schema was admitted.
func (a *Agent) restoreDiscoveredFromStore() {
	a.mu.Lock()
	p := a.persistence
	a.mu.Unlock()
	if p == nil {
		return
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		return
	}
	for _, record := range records {
		event, ok := record.Event.(*session.ToolsDiscovered)
		if !ok {
			continue
		}
		for _, tool := range event.Tools {
			if tool.ToolID != "" {
				a.markDiscovered(tool.ToolID)
			}
		}
	}
}
