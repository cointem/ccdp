package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

// SubagentDefaultSystem is the default system prompt for sub-agents launched
// via the Task tool (Claude Code's Task tool idea). Like Claude Code's
// sub-agents, sub-agents do NOT load project instruction files (the parent
// already holds that context); they get the task, workspace and environment
// details, and the parent's brief is expected to carry the specifics.
const SubagentDefaultSystem = `You are a ccdp sub-agent, an independent worker launched by the main agent.
You share the main agent's tools, working directory, sandbox and permissions.

Operating rules:
- Complete the assigned task as fully and independently as you can. You cannot
  ask the user questions or request approval; work within the permissions you
  inherit. If a tool is denied, adapt and continue instead of retrying it.
- The parent's brief is self-contained: work from what it says, plus what you
  verify yourself with tools. Do not assume knowledge of the parent's
  conversation beyond the brief.
- Read before you write. Prefer small, verifiable changes. Verify your work
  (builds, tests, re-reading the edited region) when the task allows it.
- Stay inside the scope of the brief. Fixing an unrelated bug you stumbled
  upon is out of scope — mention it in your report instead.
- Use absolute file paths in all tool arguments.

Reporting rules:
- End with a concise final answer the parent can act on directly:
  what you did, the file paths (with line numbers where useful), evidence
  (command output or test names), and anything left unfinished.
- Never claim to have performed actions you did not actually perform.
- If you failed, say so plainly with the exact error; a failed honest report
  is more useful to the parent than a confident guess.`

// runSubagents executes a batch of sub-agents concurrently. Each runs its own
// loop via runSubagent; results come back in input order. The parent's event
// stream, approval gate and usage accounting are safe for concurrent use.
func (a *Agent) runSubagents(tasks []tools.SubagentTask) ([]tools.SubagentResult, error) {
	results := make([]tools.SubagentResult, len(tasks))
	if len(tasks) == 1 {
		out, err := a.runSubagent(tasks[0].Description, tasks[0].SystemPrompt)
		results[0] = tools.SubagentResult{
			Index: 0, Description: tasks[0].Description,
			Output: out, Error: errString(err),
		}
		return results, nil
	}

	a.emitStatus("launching %d sub-agents in parallel…", len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(i int, t tools.SubagentTask) {
			defer wg.Done()
			out, err := a.runSubagent(t.Description, t.SystemPrompt)
			results[i] = tools.SubagentResult{
				Index: i, Description: t.Description,
				Output: out, Error: errString(err),
			}
		}(i, t)
	}
	wg.Wait()
	return results, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// maxSubagentSteps caps the tool-call iterations in one sub-agent run so a
// runaway delegation cannot loop forever.
const maxSubagentSteps = 50

// runSubagent executes an independent sub-agent loop, firing the SubagentStop
// hook with the accumulated output when it finishes.
func (a *Agent) runSubagent(description, systemPrompt string) (string, error) {
	a.hooks.SubagentStart(a.turnCtx, description)
	out, err := a.runSubagentInner(description, systemPrompt)
	if a.turnCtx != nil {
		a.hooks.SubagentStop(a.turnCtx, out)
	}
	return out, err
}

// runSubagentInner is the actual loop.
func (a *Agent) runSubagentInner(description, systemPrompt string) (string, error) {
	if systemPrompt == "" {
		systemPrompt = SubagentDefaultSystem
	}
	sys := systemPrompt +
		fmt.Sprintf("\n\nWorkspace: %s\nPermission mode: %s\nTask: %s",
			a.cfg.Workspace, a.perms.CurrentMode(), description)
	if info, err := workspace.CachedScan(a.cfg.Workspace, false); err == nil && info != nil {
		sys += "\n\n" + info.RepoMap(80)
	}

	a.emitStatus("subagent: %s", truncateResult(description, 60))
	history := []messages.Message{{
		Role:      messages.RoleUser,
		Content:   description,
		CreatedAt: time.Now(),
	}}

	var out strings.Builder
	for step := 0; step < maxSubagentSteps; step++ {
		if a.interrupted() {
			return out.String(), fmt.Errorf("subagent interrupted")
		}
		req := a.buildRequestFrom(sys, history)

		var (
			res  llm.StreamResult
			err  error
			text strings.Builder
			done = make(chan struct{})
		)
		// Inherit the parent turn's context when one is live; a nil context
		// would panic inside net/http.
		sctx := a.turnCtx
		if sctx == nil {
			sctx = context.Background()
		}
		go func() {
			res, err = a.client.Stream(sctx, req, func(delta string) {
				text.WriteString(delta)
				out.WriteString(delta)
			})
			close(done)
		}()
		<-done

		if err != nil {
			if a.interrupted() {
				return out.String(), fmt.Errorf("subagent interrupted")
			}
			return out.String(), fmt.Errorf("subagent LLM error: %v", err)
		}
		if res.PromptTokens > 0 || res.CompletionTok > 0 {
			a.recordUsage(res.PromptTokens, res.CompletionTok, res.CachedTokens)
		}

		var calls []messages.ToolCall
		for _, rc := range res.ToolCalls {
			calls = append(calls, messages.ToolCall{
				ID:        rc.ID,
				Name:      rc.Function.Name,
				Arguments: llm.UnmarshalArgs(rc.Function.Arguments.String()),
			})
		}
		history = append(history, messages.AssistantWithTools(text.String(), calls))

		if len(calls) == 0 {
			return out.String(), nil
		}
		for _, tc := range calls {
			if a.interrupted() {
				return out.String(), fmt.Errorf("subagent interrupted")
			}
			result, isErr := a.executeTool(tc)
			history = append(history, messages.NewToolResult(tc, result, isErr))
		}
	}
	return out.String(), fmt.Errorf("subagent exceeded %d steps", maxSubagentSteps)
}
