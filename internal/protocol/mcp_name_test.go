package protocol

import "testing"

func TestMCPToolNameQualification(t *testing.T) {
	name := MCPToolName("files", "read")
	if name != "mcp__files__read" {
		t.Fatalf("MCPToolName = %q, want mcp__files__read", name)
	}
	if !IsMCPToolName(name) {
		t.Fatalf("IsMCPToolName(%q) = false, want true", name)
	}
	for _, builtin := range []string{"Read", "Write", "Bash"} {
		if IsMCPToolName(builtin) {
			t.Errorf("built-in %q must not be reported as an MCP name", builtin)
		}
	}
}
