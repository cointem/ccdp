package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

// Stable instructions and session environment are all cacheable.
func TestSystemBlockClassification(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()

	cfg := cloneConfig(ag.services.cfg)
	cfg.Workspace = t.TempDir()

	blocks := buildSystemBlocks(cfg, time.Now(), "session-x", "PROJECT_RULES_MARKER", "SKILLS_MARKER")

	if len(blocks) == 0 {
		t.Fatal("no system blocks produced")
	}
	if blocks[0].Text != cfg.SystemPrompt || !blocks[0].Cacheable {
		t.Fatalf("first block must be the cacheable core prompt: %+v", blocks[0])
	}

	var projectFound, envFound bool
	for _, b := range blocks {
		switch {
		case strings.Contains(b.Text, "PROJECT_RULES_MARKER"):
			projectFound = true
			if !b.Cacheable {
				t.Fatalf("project instructions must be a cacheable section: %+v", b)
			}
		case strings.Contains(b.Text, "SKILLS_MARKER"):
			if !b.Cacheable {
				t.Fatalf("skills index must be a cacheable section: %+v", b)
			}
		case strings.Contains(b.Text, "# Environment"):
			envFound = true
			if !b.Cacheable {
				t.Fatalf("session environment must be cacheable: %+v", b)
			}
		}
	}
	if !projectFound {
		t.Fatalf("project instructions missing from blocks")
	}
	if !envFound {
		t.Fatalf("environment section missing from blocks")
	}
}

func TestRuntimeContextPreservesRequestPrefixAndRepositoryQuery(t *testing.T) {
	a := newAgentForTest(t)
	defer a.Close()
	cfg := cloneConfig(a.services.cfg)
	for i := 0; i < 122; i++ {
		if err := os.WriteFile(filepath.Join(cfg.Workspace, fmt.Sprintf("file%03d.go", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	workspace.Invalidate(cfg.Workspace)
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "file120"}); err != nil {
		t.Fatal(err)
	}
	take := func() stepRuntime {
		step, err := a.beginStepChecked()
		if err != nil {
			t.Fatal(err)
		}
		a.releaseStepContext(step)
		return step
	}
	first := take()
	before := a.buildRequestForStep(first)
	if wire := fmt.Sprint(before.Messages); !strings.Contains(wire, "Permission mode:") || !strings.Contains(wire, "# Repository layout") {
		t.Fatal("model request lost runtime context")
	}
	visible, _ := a.transcript.Snapshot()
	if len(visible) != 1 || visible[0].Kind != "user" || visible[0].Text != "file120" {
		t.Fatalf("runtime context leaked into the conversation: %+v", visible)
	}
	if len(first.contextMessages) != 0 {
		t.Fatal("normal step left uncommitted context")
	}
	// Runtime results and invalidated file inventories do not change the task
	// query or move context ahead of the cached conversation.
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, InternalSource: "process_completion", Content: "file121"}); err != nil {
		t.Fatal(err)
	}
	workspace.Invalidate(cfg.Workspace)
	second := take()
	after := a.buildRequestForStep(second)
	if !reflect.DeepEqual(before.SystemBlocks, after.SystemBlocks) || !reflect.DeepEqual(before.Messages, after.Messages[:len(before.Messages)]) {
		t.Fatal("runtime result rewrote the previous request prefix")
	}
	if len(second.history) != len(first.history)+1 {
		t.Fatal("unchanged context was appended again")
	}
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "file121"}); err != nil {
		t.Fatal(err)
	}
	third := take()
	var maps []messages.Message
	for _, m := range third.history {
		if m.InternalSource == "runtime_repository" {
			maps = append(maps, m)
		}
	}
	if len(maps) != 2 || maps[0].ContextKey == maps[1].ContextKey || maps[0].Content == maps[1].Content {
		t.Fatalf("new user query did not append a new relevant map: %+v", maps)
	}
	if !reflect.DeepEqual(first.systemBlocks, third.systemBlocks) {
		t.Fatal("user query changed system prefix")
	}
	encoded, err := session.EncodeMessage(maps[1])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := session.DecodeMessage(encoded)
	if err != nil || !reflect.DeepEqual(maps[1], decoded) {
		t.Fatal("runtime context identity lost during replay", err)
	}
}

func TestCompactionPreservesUserIntentBeforeRuntimeContext(t *testing.T) {
	history := []messages.Message{{Role: messages.RoleUser, ID: "user", Content: "exact task"}}
	for i := 0; i < 20; i++ {
		history = append(history, messages.Message{Role: messages.RoleAssistant, Content: strings.Repeat("work ", 100)})
	}
	history = append(history, messages.Message{Role: messages.RoleUser, InternalSource: "runtime_goal", Content: "current budget"})
	_, _, kept, _, ok := compactHistorySpans(history, 2, 100)
	if !ok {
		t.Fatal("fixture was not compacted")
	}
	for _, m := range kept {
		if m.ID == "user" {
			return
		}
	}
	t.Fatal("runtime state displaced the latest real user input")
}

func TestContextSnapshotDoesNotAppendRuntimeHistory(t *testing.T) {
	a := newAgentForTest(t)
	defer a.Close()
	before := a.History()
	step, req, err := a.snapshotContextRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(step.contextMessages) == 0 || !strings.Contains(fmt.Sprint(req.Messages), "# Repository layout") {
		t.Fatal("compaction preview lacks runtime context")
	}
	if !reflect.DeepEqual(before, a.History()) {
		t.Fatal("preview mutated history")
	}
}
