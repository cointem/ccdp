package agent

import "ccdp/internal/tools"

// planAllowedImplementation classifies the concrete implementation, not its
// advertised name. A plugin or project command named "Read" must not inherit
// the built-in Read capability in plan mode (and likewise for every other
// allowlisted name).
func planAllowedImplementation(tool tools.Tool) bool {
	switch tool.(type) {
	case *tools.ReadTool, *tools.GlobTool, *tools.GrepTool, *tools.LSTool,
		*tools.GitStatusTool, *tools.GitDiffTool, *tools.GitLogTool,
		*tools.ReadSkillTool, *toolSearchTool,
		*tools.TodoWriteTool, *tools.TaskTool, *tools.AgentTool,
		*tools.WebFetchTool, *tools.WebSearchTool,
		*enterPlanModeTool, *exitPlanModeTool, *askUserQuestionTool:
		return true
	default:
		return false
	}
}

// readOnlyImplementation is the stricter classification used by the
// concurrent dispatch pool. TodoWrite, Task and web tools remain barriers
// even though the historical plan workflow permits their real built-ins.
func readOnlyImplementation(tool tools.Tool) bool {
	switch tool.(type) {
	case *tools.ReadTool, *tools.GlobTool, *tools.GrepTool, *tools.LSTool,
		*tools.GitStatusTool, *tools.GitDiffTool, *tools.GitLogTool,
		*tools.ReadSkillTool, *toolSearchTool:
		return true
	default:
		return false
	}
}
