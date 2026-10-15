package agent

import (
	"ccdp/internal/permissions"
	"ccdp/internal/tools"
)

// toolKind is the sole built-in implementation classification. Names remain
// labels for user rules; a plugin shadowing Read never inherits its capability.
func toolKind(tool tools.Tool) permissions.ToolKind {
	switch tool.(type) {
	case *tools.ReadTool, *tools.GlobTool, *tools.RgTool, *tools.LSTool,
		*tools.ReadSkillTool, *tools.CodeNavigateTool, *toolSearchTool:
		return permissions.KindRead
	case *tools.TodoWriteTool, *tools.CreateGoalTool, *tools.GetGoalTool, *tools.UpdateGoalTool,
		*enterPlanModeTool, *exitPlanModeTool, *askUserQuestionTool:
		return permissions.KindState
	case *tools.WriteTool, *tools.EditTool:
		return permissions.KindWrite
	case *tools.BashTool:
		return permissions.KindShell
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

func planAllowedImplementation(tool tools.Tool) bool { return toolKind(tool).PlanAllowed() }

func readOnlyImplementation(tool tools.Tool) bool {
	kind := toolKind(tool)
	return kind == permissions.KindRead || kind == permissions.KindNetwork
}
