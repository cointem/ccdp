package agent

// snapshotGoalRuns releases the supervisor lock before inspecting any run.
// Run ownership is checked under run.mu, never while Agent.mu is held.
func (a *Agent) snapshotGoalRuns() []*managedRun {
	if a.services.supervisor == nil {
		return nil
	}
	runs := make([]*managedRun, 0, len(a.services.supervisor.directory.Runs()))
	for _, run := range a.services.supervisor.directory.Runs() {
		runs = append(runs, run)
	}
	return runs
}

// cancelGoalChildren stops every active descendant charged to this goal,
// regardless of join/notify/observe policy. Runs belonging to another goal or
// ordinary work are unaffected, including runs reusing a child session id.
func (a *Agent) cancelGoalChildren(id string) {
	if id == "" {
		return
	}
	for _, run := range a.snapshotGoalRuns() {
		run.mu.Lock()
		if run.opts.goalOwner == a && run.opts.goalOwnerID == id && run.fact.Child.Run.Active() && run.cancel != nil {
			run.cancel()
		}
		run.mu.Unlock()
	}
}

func (a *Agent) goalChildrenActive() bool {
	a.mu.Lock()
	id := ""
	if a.conversation.durable.Goal != nil {
		id = a.conversation.durable.Goal.ID
	}
	a.mu.Unlock()
	if id == "" {
		return false
	}
	for _, run := range a.snapshotGoalRuns() {
		run.mu.Lock()
		active := run.opts.goalOwner == a && run.opts.goalOwnerID == id && run.fact.Child.Run.Active()
		run.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}
