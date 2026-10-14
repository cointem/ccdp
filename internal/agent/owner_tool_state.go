package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

type sessionCapabilitiesRequested struct {
	version      capabilityPolicyVersion
	capabilities []protocol.CapabilityRequest
	result       chan sessionCapabilitiesResult
}

func (sessionCapabilitiesRequested) ownerControlMessage() {}

type sessionCapabilitiesResult struct {
	policy  *sandbox.Sandbox
	version capabilityPolicyVersion
	err     error
}

func (a *Agent) addSessionCapabilities(version capabilityPolicyVersion, requested []protocol.CapabilityRequest) (*sandbox.Sandbox, capabilityPolicyVersion, error) {
	if a.owner == nil {
		return a.addSessionCapabilitiesOwned(version, requested)
	}
	request := sessionCapabilitiesRequested{version: version, capabilities: append([]protocol.CapabilityRequest(nil), requested...), result: make(chan sessionCapabilitiesResult, 1)}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return nil, version, session.ErrClosed
	}
	select {
	case result := <-request.result:
		return result.policy, result.version, result.err
	case <-a.owner.done:
		return nil, version, session.ErrClosed
	}
}

type guardianSlotRequested struct{ result chan bool }

func (guardianSlotRequested) ownerControlMessage() {}
func (a *Agent) takeGuardianSlotOwned() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turnMemory.guardianUses >= 3 {
		return false
	}
	a.turnMemory.guardianUses++
	return true
}
func (a *Agent) takeGuardianSlot() bool {
	if a.owner == nil {
		return a.takeGuardianSlotOwned()
	}
	a.ensureRun()
	request := guardianSlotRequested{result: make(chan bool, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return false
	}
	select {
	case admitted := <-request.result:
		return admitted
	case <-a.owner.done:
		return false
	}
}

type toolOutputRequested struct {
	text   string
	result chan string
}

func (toolOutputRequested) ownerControlMessage() {}
func (a *Agent) clipAggregate(out string) string {
	if a.owner == nil {
		return a.clipAggregateOwned(out)
	}
	a.ensureRun()
	request := toolOutputRequested{text: out, result: make(chan string, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return "session closed before tool output projection"
	}
	select {
	case text := <-request.result:
		return text
	case <-a.owner.done:
		return "session closed before tool output projection"
	}
}

type toolsDiscoveredPrepared struct {
	version uint64
	schemas []session.ToolSchema
	result  chan error
}

func (toolsDiscoveredPrepared) ownerControlMessage() {}
func (a *Agent) installDiscoveredTools(request toolsDiscoveredPrepared) error {
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	if err := p.PersistToolsDiscovered(request.version, request.schemas); err != nil {
		return a.toolJournalFailure(err)
	}
	a.mu.Lock()
	for _, schema := range request.schemas {
		a.discovery.discovered[schema.ToolID] = true
	}
	a.mu.Unlock()
	return nil
}
func (a *Agent) discoverToolsFromWorker(version uint64, schemas []session.ToolSchema) error {
	request := toolsDiscoveredPrepared{version: version, schemas: schemas, result: make(chan error, 1)}
	if a.owner == nil {
		return a.installDiscoveredTools(request)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}

type tasksPrepared struct {
	tasks  []session.Task
	result chan error
}

func (tasksPrepared) ownerControlMessage() {}
func (a *Agent) commitTasks(request tasksPrepared) error {
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	return a.toolJournalFailure(p.PersistTasks(request.tasks))
}
func (a *Agent) saveTasksFromWorker(tasks []session.Task) error {
	request := tasksPrepared{tasks: tasks, result: make(chan error, 1)}
	if a.owner == nil {
		return a.commitTasks(request)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}
