package agent

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/tools"
)

// buildSystemBlocks contains only stable instructions and session identity.
// Runtime state is appended to history so it cannot invalidate the system and
// conversation prefix on every tool round.
func buildSystemBlocks(cfg config.Config, sessionStartAt time.Time, sessionID, instructions, skillsSection string) []llm.SystemBlock {
	blocks := make([]llm.SystemBlock, 0, 4)
	add := func(text string) {
		if text != "" {
			blocks = append(blocks, llm.SystemBlock{Text: text, Cacheable: true})
		}
	}
	add(cfg.SystemPrompt)
	add(skillsSection)
	if instructions != "" {
		add("# Project instructions\n\n" + instructions)
	}
	add(fmt.Sprintf("# Environment\n- Date: %s\n- Timezone: %s\n- OS: %s\n- Shell: %s\n- Workspace: %s\n- Session: %s",
		sessionStartAt.Format("2006-01-02 15:04:05"), time.Local.String(), runtime.GOOS, shellName(), cfg.Workspace, sessionID))
	return blocks
}

// joinSystemBlocks concatenates the ordered system sections into the single
// system string used by the chat/responses wires.
func joinSystemBlocks(blocks []llm.SystemBlock) string {
	var sb strings.Builder
	for i, b := range blocks {
		if i > 0 && !strings.HasPrefix(b.Text, "\n") {
			sb.WriteString("\n\n")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

func (a *Agent) todoSection() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	states := make([]tools.TodoState, 0, len(a.conversation.durable.Tasks))
	for _, task := range a.conversation.durable.Tasks {
		states = append(states, tools.TodoState{ID: task.TaskID, Content: task.Title, Status: task.Status, Priority: task.Priority})
	}
	return tools.TodoSection(states)
}

// shellName returns the user's shell for the environment section.
func shellName() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}
