package tools

import "testing"

// Wrapping an implementation in the basic Tool interface does not implicitly
// grant the optional program capability, even when its name is a built-in.
type unmarkedTool struct{ Tool }
type mutableProgramTool struct {
	Tool
	enabled bool
}

func (t *mutableProgramTool) ProgramCallable() bool { return t.enabled }

func TestProgramCapabilityIsExplicitAndFrozen(t *testing.T) {
	r := NewRegistry()
	for _, tool := range []Tool{&unmarkedTool{&ReadTool{}}, &RunCodeTool{}, &TodoWriteTool{}, NewAgentTool("SpawnAgent")} {
		r.Register(tool)
		lease := r.Acquire()
		d, _ := lease.Definition(tool.Name())
		if d.ProgramCallable {
			t.Fatalf("implicit program grant: %s", tool.Name())
		}
	}
	tool := &mutableProgramTool{Tool: &ReadTool{}, enabled: true}
	r.Register(tool)
	lease := r.Acquire()
	tool.enabled = false
	d, _ := lease.Definition("Read")
	if !d.ProgramCallable {
		t.Fatal("live mutation changed frozen program grant")
	}
}
