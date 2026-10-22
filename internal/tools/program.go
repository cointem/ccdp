package tools

// Explicit opt-ins for ordinary operations. Session, plan, goal and agent
// control tools deliberately do not implement ProgramTool.
func (*ReadTool) ProgramCallable() bool         { return true }
func (*WriteTool) ProgramCallable() bool        { return true }
func (*EditTool) ProgramCallable() bool         { return true }
func (*GlobTool) ProgramCallable() bool         { return true }
func (*RgTool) ProgramCallable() bool           { return true }
func (*LSTool) ProgramCallable() bool           { return true }
func (*BashTool) ProgramCallable() bool         { return true }
func (*ProcessTool) ProgramCallable() bool      { return true }
func (*ReadOutputTool) ProgramCallable() bool   { return true }
func (*ReadSkillTool) ProgramCallable() bool    { return true }
func (*CodeNavigateTool) ProgramCallable() bool { return true }
func (*WebFetchTool) ProgramCallable() bool     { return true }
func (*WebSearchTool) ProgramCallable() bool    { return true }
func (*CommandTool) ProgramCallable() bool      { return true }
