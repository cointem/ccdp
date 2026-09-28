package agent

import (
	"errors"
	"fmt"

	"ccdp/internal/messages"
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
const SubagentDefaultSystem = `You are a ccdp sub-agent, an independent worker launched by the main agent.
You have an explicit task role, shared or isolated workspace and a frozen permission snapshot, and
have your own session, history, resources and cancellation boundary. Mutable
project hooks, MCP startup and memory are not inherited.

Operating rules:
- Complete the assigned task independently. Tool approvals are routed to the
  human when an interactive approval channel is enabled. In headless mode,
  calls that require approval are denied; report the limitation and continue
  when possible. Never treat an approval as overriding inherited hard denies.
- The parent's brief is self-contained: work from what it says and verify
  facts with the tools available in this child session.
- Read before writing. Prefer small, verifiable changes and report evidence.
- Stay inside the scope of the brief. Never launch another child.
- Use SendMessage to parent for important discoveries, coordination or blockers while continuing your work. Sending a message does not end your task.

Reporting rules:
- End with a concise final answer the parent can act on directly: what you did,
  files changed, evidence (commands/tests), and anything unfinished.
- Never claim to have performed actions you did not perform.`

func (a *Agent) spawnAgent(request tools.SpawnAgentRequest, callID string) (protocol.AgentReceipt, error) {
	if a.childState != nil {
		return protocol.AgentReceipt{}, errors.New("nested delegation is not available")
	}
	task := childTask{Description: request.Task, Name: request.Name, Role: request.Role, WorkspaceMode: request.Workspace, ContextMode: request.Context, WaitPolicy: "notify"}
	r, err := a.supervisor.launch(a, parentTurnContext(a), task, childPurposeTask, callID)
	if err != nil {
		return protocol.AgentReceipt{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return protocol.AgentReceipt{AgentID: r.fact.Child.SessionID, Status: "accepted"}, nil
}

// Guardian is an internal synchronous inspection, driven by the same supervisor.
func (a *Agent) runGuardianChild(description string) (string, error) {
	if a.childState != nil {
		return "", errors.New("nested guardian is not available")
	}
	r, err := a.supervisor.launch(a, parentTurnContext(a), childTask{Description: description, SystemPrompt: GuardianSystem}, childPurposeGuardian, "")
	if err != nil {
		return "", err
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fact.Child.Run.Output, r.err
}

func childBudgetError(child *Agent, view protocol.SessionView) error {
	child.mu.Lock()
	cfg := cloneConfig(child.cfg)
	child.mu.Unlock()
	history := child.History()
	if cfg.MaxBudgetUSD > 0 && view.Usage.Cost >= cfg.MaxBudgetUSD {
		return fmt.Errorf("child exceeded budget ($%.2f of $%.2f)", view.Usage.Cost, cfg.MaxBudgetUSD)
	}
	if cfg.MaxTurns > 0 && len(history) > 0 {
		modelCalls := 0
		latestAssistantHasTools := false
		for i := len(history) - 1; i >= 0; i-- {
			message := history[i]
			if message.Role != messages.RoleAssistant {
				continue
			}
			if modelCalls == 0 {
				latestAssistantHasTools = len(message.ToolCalls) > 0
			}
			if len(message.ToolCalls) > 0 {
				modelCalls++
			}
		}
		if latestAssistantHasTools && modelCalls >= cfg.MaxTurns {
			return fmt.Errorf("child exceeded %d turns", cfg.MaxTurns)
		}
	}
	return nil
}
