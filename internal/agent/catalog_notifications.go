package agent

import (
	"ccdp/internal/events"
)

// Catalog notifications carry no state: observers reread the versioned catalog.
// Coalescing them in one session-owned worker keeps plugin callbacks outside
// the owner and MCP/registry publication locks. Cancellation joins this worker
// before disposing the plugin host.
func (a *Agent) startCatalogNotifications() {
	changes := make(chan struct{}, 1)
	done := make(chan struct{})
	a.catalogNotificationsDone = done
	a.services.registry.SetChangeListener(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	ctx, bus := a.lifecycle.rootCtx, a.services.evbus
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-changes:
				if ctx.Err() != nil {
					return
				}
				bus.Emit(events.TopicToolChange, events.ToolEvent{})
			}
		}
	}()
}
