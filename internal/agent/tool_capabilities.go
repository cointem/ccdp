package agent

import (
	"ccdp/internal/permissions"
	"ccdp/internal/tools"
)

// toolKind is the sole built-in implementation classification. Names remain
// labels for user rules; a plugin shadowing Read never inherits its capability.
func toolKind(tool tools.Tool) permissions.ToolKind {
	switch tool.(type) {
	case *tools.RunCodeTool:
		return permissions.KindOrchestration
	case *tools.ReadTool, *tools.GlobTool, *tools.RgTool, *tools.LSTool,
		*tools.ReadSkillTool, *tools.CodeNavigateTool, *toolSearchTool, *tools.ReadOutputTool, *readProjectMemoryTool:
		return permissions.KindRead
	case *tools.TodoWriteTool, *tools.CreateGoalTool, *tools.GetGoalTool, *tools.UpdateGoalTool,
		*askUserQuestionTool:
		return permissions.KindState
	case *tools.WriteTool, *tools.EditTool, *updateProjectMemoryTool:
		return permissions.KindWrite
	case *tools.BashTool:
		return permissions.KindShell
	case *tools.ComputerTool:
		return permissions.KindComputer
	case *tools.ProcessTool:
		return permissions.KindProcess
	case *tools.AgentTool:
		return permissions.KindDelegate
	case *tools.WebFetchTool, *tools.WebSearchTool:
		return permissions.KindNetwork
	default:
		return permissions.KindUnknown
	}
}

func readOnlyImplementation(tool tools.Tool) bool {
	kind := toolKind(tool)
	return kind == permissions.KindRead || kind == permissions.KindNetwork
}

// Process observations and cleanup do not grant authority to a new command.
// The concrete implementation and its owner-scoped handle still gate access.
func invocationKind(tool tools.Tool, args map[string]any) permissions.ToolKind {
	if _, ok := tool.(*tools.ProcessTool); ok {
		switch tools.StringArg(args, "action", "") {
		case "read", "wait":
			return permissions.KindProcessRead
		case "stop":
			return permissions.KindProcessStop
		}
	}
	return toolKind(tool)
}

// These operate on runtime-owned state or already-owned process handles.
// They remain available when project execution has been quarantined.
func controlTool(tool tools.Tool, args map[string]any) bool {
	switch tool.(type) {
	case *tools.ReadOutputTool, *toolSearchTool:
		return true
	}
	kind := invocationKind(tool, args)
	return kind == permissions.KindState || kind == permissions.KindProcessRead || kind == permissions.KindProcessStop
}
