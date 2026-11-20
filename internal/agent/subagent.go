package agent

import (
	"context"
	"errors"
	"fmt"

	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

// childTask is the supervisor's internal launch specification. It is not a tool
// schema; review and guardian use internal delivery and system-prompt settings.
type childTask struct {
	Role          string
	WorkspaceMode string
	ContextMode   string
	Name          string
	WaitPolicy    string
	Description   string
	SystemPrompt  string
}

// SubagentDefaultSystem is the default system prompt for a task child. Child
// sessions do not reload project instructions; the parent already froze the
// effective configuration and the child constructor appends the usual
// workspace/runtime context.
const SubagentDefaultSystem = `Complete the task assigned by your parent within your inherited permissions.
Use the conversation history as background and verify current files before editing.
Coordinate with your parent via SendMessage when a discovery or blocker affects its work.
Your final answer is delivered automatically. Summarize the outcome, changed paths,
verification and any unfinished work. Only report actions you performed.`

func (a *Agent) spawnAgent(request tools.SpawnAgentRequest, callID string) (protocol.AgentReceipt, error) {
	if a.children.childState != nil {
		return protocol.AgentReceipt{}, errors.New("nested delegation is not available")
	}
	task := childTask{Description: request.Task, Name: request.Name, Role: request.Role, WorkspaceMode: request.Workspace, ContextMode: request.Context, WaitPolicy: "notify"}
	r, err := a.services.supervisor.launch(a, parentTurnContext(a), task, childPurposeTask, callID)
	if err != nil {
		return protocol.AgentReceipt{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return protocol.AgentReceipt{AgentID: r.fact.Child.SessionID, Status: "accepted"}, nil
}

// Guardian is an internal synchronous inspection, driven by the same supervisor.
func (a *Agent) runGuardianChild(ctx context.Context, description string) (string, error) {
	if a.children.childState != nil {
		return "", errors.New("nested guardian is not available")
	}
	if ctx == nil {
		ctx = parentTurnContext(a)
	}
	r, err := a.services.supervisor.launch(a, ctx, childTask{Description: description, SystemPrompt: GuardianSystem}, childPurposeGuardian, "")
	if err != nil {
		return "", err
	}
	<-r.done
	if err := a.persistenceFailure(); err != nil {
		return "", &guardianBoundaryError{fmt.Errorf("guardian outcome persistence failed: %w", err)}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.executionBoundaryErr != nil {
		return "", &guardianBoundaryError{r.executionBoundaryErr}
	}
	return r.fact.Child.Run.Output, r.err
}
