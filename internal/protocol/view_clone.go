package protocol

import "slices"

// CloneSessionView transfers a view to a reader without sharing writable
// slices or decision values with the publisher or another reader.
func CloneSessionView(v SessionView) SessionView {
	v.Goal = CloneGoal(v.Goal)
	v.Question = CloneQuestionRequest(v.Question)
	v.Transcript = slices.Clone(v.Transcript)
	for i := range v.Transcript {
		v.Transcript[i].Args = slices.Clone(v.Transcript[i].Args)
	}
	v.History = slices.Clone(v.History)
	for i := range v.History {
		v.History[i].ToolCallIDs = slices.Clone(v.History[i].ToolCallIDs)
		v.History[i].ToolCalls = slices.Clone(v.History[i].ToolCalls)
	}
	v.PendingInputs = slices.Clone(v.PendingInputs)
	v.Tasks = slices.Clone(v.Tasks)
	if v.Approval != nil {
		copy := *v.Approval
		copy.Args = slices.Clone(copy.Args)
		copy.Capabilities = slices.Clone(copy.Capabilities)
		v.Approval = &copy
	}
	if v.Plan != nil {
		copy := *v.Plan
		v.Plan = &copy
	}
	if v.LastTurn != nil {
		copy := *v.LastTurn
		v.LastTurn = &copy
	}
	v.Settings.Permission.AlwaysAllow = slices.Clone(v.Settings.Permission.AlwaysAllow)
	v.Settings.Permission.AlwaysDeny = slices.Clone(v.Settings.Permission.AlwaysDeny)
	v.Settings.Sandbox.AdditionalDirectories = slices.Clone(v.Settings.Sandbox.AdditionalDirectories)
	v.Settings.Sandbox.AdditionalReadOnlyDirectories = slices.Clone(v.Settings.Sandbox.AdditionalReadOnlyDirectories)
	v.Settings.Sandbox.DisallowedDirectories = slices.Clone(v.Settings.Sandbox.DisallowedDirectories)
	v.SandboxRuntime.SessionGrants = slices.Clone(v.SandboxRuntime.SessionGrants)
	v.Catalog.Providers = slices.Clone(v.Catalog.Providers)
	for i := range v.Catalog.Providers {
		v.Catalog.Providers[i].Models = slices.Clone(v.Catalog.Providers[i].Models)
		v.Catalog.Providers[i].Capabilities = slices.Clone(v.Catalog.Providers[i].Capabilities)
	}
	v.Catalog.Tools = slices.Clone(v.Catalog.Tools)
	for i := range v.Catalog.Tools {
		v.Catalog.Tools[i].Effects = slices.Clone(v.Catalog.Tools[i].Effects)
		v.Catalog.Tools[i].Schema = slices.Clone(v.Catalog.Tools[i].Schema)
	}
	return v
}

// CloneUpdate gives each subscription its own writable protocol values.
// Updates form a tree: a child update may contain another update, never its
// enclosing update. Strings and immutable scalar values remain shared.
func CloneUpdate(u Update) Update {
	if u.Snapshot != nil {
		value := CloneSessionView(*u.Snapshot)
		u.Snapshot = &value
	}
	if u.State != nil {
		var view SessionView
		view.ApplyState(*u.State)
		state := CloneSessionView(view).State()
		u.State = &state
	}
	if u.Catalog != nil {
		catalog := CloneSessionView(SessionView{Catalog: *u.Catalog}).Catalog
		u.Catalog = &catalog
	}
	if u.Receipt != nil {
		receipt := *u.Receipt
		if receipt.Error != nil {
			value := *receipt.Error
			receipt.Error = &value
		}
		u.Receipt = &receipt
	}
	if u.Event != nil {
		event := *u.Event
		view := CloneSessionView(SessionView{Goal: event.Goal, Question: event.Question, Approval: event.Approval, Plan: event.Plan})
		event.Goal, event.Question, event.Approval, event.Plan = view.Goal, view.Question, view.Approval, view.Plan
		if event.Transcript != nil {
			item := *event.Transcript
			item.Args = slices.Clone(item.Args)
			event.Transcript = &item
		}
		if event.Tool != nil {
			tool := *event.Tool
			tool.Args = slices.Clone(tool.Args)
			event.Tool = &tool
		}
		if event.Usage != nil {
			usage := *event.Usage
			event.Usage = &usage
		}
		u.Event = &event
	}
	if u.Child != nil {
		child := *u.Child
		view := CloneSessionView(SessionView{Question: child.Session.Question, Approval: child.Session.Approval, Plan: child.Session.Plan})
		child.Session.Question, child.Session.Approval, child.Session.Plan = view.Question, view.Approval, view.Plan
		if child.Update != nil {
			update := CloneUpdate(*child.Update)
			child.Update = &update
		}
		u.Child = &child
	}
	return u
}
