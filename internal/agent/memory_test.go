package agent

import (
	"os"
	"strings"
	"testing"

	"ccdp/internal/config"
)

// newMemoryAgent builds an agent with AutoMem enabled and a per-test session
// directory.
func newMemoryAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.EnableMemory = config.BoolPtr(true)
	events := make(chan Event, 64)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestMemoryDisabledRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.EnableMemory = config.BoolPtr(false) // default: off
	events := make(chan Event, 64)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ag.Close()

	ag.mu.Lock()
	ag.turnUserMsg = "please add a feature"
	ag.turnTouched = []string{"a.go"}
	ag.turnSummary = "done"
	ag.mu.Unlock()
	ag.recordMemory()

	if _, err := os.Stat(ag.memoryFile()); !os.IsNotExist(err) {
		t.Fatal("memory file should not exist when enable_memory is off")
	}
}

func TestRecordMemoryWritesFileAndInjects(t *testing.T) {
	ag := newMemoryAgent(t)
	defer ag.Close()

	ag.mu.Lock()
	ag.turnUserMsg = "refactor the http handler"
	ag.turnTouched = []string{"server.go", "client.go"}
	ag.turnSummary = "Split the handler into two routes and updated the client."
	ag.mu.Unlock()
	ag.recordMemory()

	text := ag.MemoryText()
	if text == "" {
		t.Fatal("memory log empty after recordMemory")
	}
	for _, want := range []string{"refactor the http handler", "client.go, server.go", "Split the handler"} {
		if !strings.Contains(text, want) {
			t.Errorf("memory log missing %q:\n%s", want, text)
		}
	}

	// Build through the same checked, frozen step boundary used by a real turn.
	step, err := ag.beginStepChecked()
	if err != nil {
		t.Fatalf("beginStepChecked: %v", err)
	}
	req, err := ag.buildRequestForStepChecked(step)
	releaseStepLease(step)
	if err != nil {
		t.Fatalf("buildRequestForStepChecked: %v", err)
	}
	// The direct test does not run the turn-finalizer that normally clears this
	// borrow; release it explicitly so no frozen child view survives the test.
	ag.mu.Lock()
	ag.childStep = nil
	ag.mu.Unlock()
	// The memory section appears in the system prompt when enabled.
	sys, _ := req.Messages[0].Content.(string)
	if !strings.Contains(sys, "# Session memory") || !strings.Contains(sys, "refactor the http handler") {
		t.Errorf("system prompt missing memory section:\n%s", sys)
	}

	// Clear empties the log.
	if err := ag.ClearMemory(); err != nil {
		t.Fatalf("ClearMemory: %v", err)
	}
	if ag.MemoryText() != "" {
		t.Error("memory text should be empty after clear")
	}
	if _, err := os.Stat(ag.memoryFile()); !os.IsNotExist(err) {
		t.Error("memory file should be removed by clear")
	}
}

func TestRecordMemoryBounded(t *testing.T) {
	ag := newMemoryAgent(t)
	defer ag.Close()

	// Simulate more turns than the cap allows.
	for i := 0; i < maxMemoryEntries+5; i++ {
		ag.mu.Lock()
		ag.turnUserMsg = "turn " + string(rune('A'+i%26)) + string(rune('0'+i%10))
		ag.turnTouched = nil
		ag.turnSummary = "summary"
		ag.mu.Unlock()
		ag.recordMemory()
	}

	// The log must stay bounded; the oldest entries are dropped.
	if got := strings.Count(ag.MemoryText(), "### "); got > maxMemoryEntries {
		t.Errorf("memory log exceeded cap: %d entries", got)
	}
	if strings.Contains(ag.MemoryText(), "turn A0") {
		t.Error("oldest memory entry should have been dropped")
	}
}

func TestRecordTouchedDeduplicates(t *testing.T) {
	ag := newMemoryAgent(t)
	defer ag.Close()

	ag.recordTouched("a.go")
	ag.recordTouched("b.go")
	ag.recordTouched("a.go")
	ag.mu.Lock()
	n := len(ag.turnTouched)
	ag.mu.Unlock()
	if n != 2 {
		t.Errorf("expected 2 unique touched files, got %d", n)
	}
}
