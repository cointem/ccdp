package agent

import (
	"fmt"

	"ccdp/internal/tools"
)

// Test convenience only: launch independent tasks through the production
// supervisor, then join them in input order to assert concurrency and outcomes.
type testChildResult struct {
	Index         int
	Description   string
	Role          string
	Workspace     string
	WorkspaceMode string
	SessionID     string
	RunID         string
	Output        string
	Error         string
}

func runTestChildren(a *Agent, tasks []childTask, callID string) ([]testChildResult, error) {
	if callID == "" {
		callID = nextRuntimeID("test-launch")
	}
	results := make([]testChildResult, len(tasks))
	runs := make([]*managedRun, len(tasks))
	for i, task := range tasks {
		results[i] = testChildResult{Index: i, Description: task.Description}
		r, err := a.supervisor.launch(a, parentTurnContext(a), task, childPurposeTask, fmt.Sprintf("%s-%d", callID, i))
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		runs[i] = r
	}
	for i, r := range runs {
		if r == nil {
			continue
		}
		<-r.done
		r.mu.Lock()
		child := r.fact.Child
		results[i].SessionID, results[i].RunID = string(child.SessionID), string(child.Run.ID)
		results[i].Role, results[i].Workspace, results[i].WorkspaceMode = child.Role, child.Workspace, child.WorkspaceMode
		results[i].Output = child.Run.Output
		if r.err != nil {
			results[i].Error = r.err.Error()
		}
		r.mu.Unlock()
	}
	return results, nil
}

func runTestChild(a *Agent, description string) (string, error) {
	rows, err := runTestChildren(a, []childTask{{Description: description}}, "")
	if err != nil {
		return "", err
	}
	if rows[0].Error != "" {
		return rows[0].Output, fmt.Errorf("child: %s", rows[0].Error)
	}
	return rows[0].Output, nil
}

// A test-only synchronous tool exercises parent step binding and hooks without
// retaining a synchronous model-facing delegation API in production.
type joinChildTestTool struct{ parent *Agent }

func (joinChildTestTool) Name() string        { return "JoinChildTest" }
func (joinChildTestTool) Description() string { return "Join one test child" }
func (joinChildTestTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"description": map[string]any{"type": "string"}}}
}
func (t joinChildTestTool) Run(ctx *tools.Context) (string, error) {
	return runTestChild(t.parent, tools.StringArg(ctx.Args, "description", ""))
}
