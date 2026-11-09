package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/fsops"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/session"
	"ccdp/internal/tooling"
)

func TestReadAuthorityUsesDeliveredContent(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		limit         int
		observed      bool
	}{
		{"omitted", "body\n", 1, false},
		{"line number only", "body\n", 2, false},
		{"partial body", "body\n", 3, true},
		{"partial multi-byte rune", "中文\n", 4, false},
		{"one multi-byte rune", "中文\n", 5, true},
		{"long line preview", strings.Repeat("x", 100000), 3, true},
		{"omitted long line preview", strings.Repeat("x", 100000), 2, false},
		{"empty file marker cut", "", 6, false},
		{"empty file", "", 128, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := capabilityTestAgent(t)
			path := filepath.Join(a.configSnapshot().Workspace, "read.txt")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			call := messages.ToolCall{ID: "read", Name: "Read", Arguments: map[string]any{"file_path": path}}
			journal := toolJournalContextForStep(testToolStep(t, a))
			journal.maxOutputBytes = tc.limit
			result := a.executeToolResultWithJournal(call, journal)
			if result.IsError {
				t.Fatal(result.Output)
			}
			if _, err := a.services.resources.Files.ObservedVersion(path); err == nil {
				t.Fatal("undelivered Read granted overwrite authority")
			}
			results := []tooling.Result{result}
			if _, err := a.projectToolResults(journal, []messages.ToolCall{call}, results); err != nil {
				t.Fatal(err)
			}
			_, err := a.services.resources.Files.ObservedVersion(path)
			if (err == nil) != tc.observed {
				t.Fatalf("observed=%v text=%q, want %v", err == nil, results[0].Output, tc.observed)
			}
		})
	}
}

func TestOmittedReadDoesNotEraseEarlierObservation(t *testing.T) {
	a := capabilityTestAgent(t)
	path := filepath.Join(a.configSnapshot().Workspace, "read.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := fsops.Observe(path)
	if err != nil {
		t.Fatal(err)
	}
	a.services.resources.Files.MarkVersion(path, old)
	if err := os.WriteFile(path, []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	call := messages.ToolCall{ID: "read", Name: "Read", Arguments: map[string]any{"file_path": path}}
	journal := toolJournalContextForStep(testToolStep(t, a))
	journal.maxOutputBytes = 2
	result := a.executeToolResultWithJournal(call, journal)
	if result.IsError {
		t.Fatal(result.Output)
	}
	if _, err := a.projectToolResults(journal, []messages.ToolCall{call}, []tooling.Result{result}); err != nil {
		t.Fatal(err)
	}
	if v, err := a.services.resources.Files.ObservedVersion(path); err != nil || v != old {
		t.Fatalf("earlier observation was erased/replaced: %v %v", v, err)
	}
}

func TestCodeModeCanReadThenRewriteExistingFile(t *testing.T) {
	a, calls, events := codeAgent(t, `try { await tools.Edit({file_path:"input.txt",edits:[{old_text:"missing",new_text:"wrong"}]}); throw Error("invalid edit succeeded"); } catch(e) { if (e.result?.failure?.side_effect !== "none" || e.result.failure.retry_class !== "after_correction") throw e; } const r=await tools.Read({file_path:"input.txt",limit:1}); await tools.Write({file_path:"input.txt",content:r.data.lines[0].text+" changed"});`, nil)
	path := filepath.Join(a.configSnapshot().Workspace, "input.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	finishCode(t, a, events)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original changed" || calls.Load() != 2 {
		t.Fatalf("dependent tools failed: %q %v requests=%d", data, err, calls.Load())
	}
}

func TestEditJournalRecordsKnownNonPublication(t *testing.T) {
	a := capabilityTestAgent(t)
	a.services.perms.SetMode(permissions.ModeAuto)
	path := filepath.Join(a.configSnapshot().Workspace, "edit.txt")
	if err := os.WriteFile(path, []byte("same\nsame\n"), 0600); err != nil {
		t.Fatal(err)
	}
	call := messages.ToolCall{ID: "edit", Name: "Edit", Arguments: map[string]any{"file_path": path, "edits": []any{map[string]any{"old_text": "same", "new_text": "changed"}}}}
	result := a.executeToolResultWithJournal(call, toolJournalContextForStep(testToolStep(t, a)))
	if !result.IsError || result.Failure == nil || result.Failure.SideEffect != "none" || !strings.Contains(result.Output, "0 edits applied") {
		t.Fatalf("%+v", result)
	}
	rows, err := a.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if fact, ok := row.Event.(*session.ToolFinished); ok && fact.CallID == call.ID {
			if fact.SideEffect != "none" || fact.Failure == nil || fact.Failure.ExecutionStarted != "yes" || fact.Failure.RetryClass != "after_correction" {
				t.Fatalf("%+v", fact)
			}
			return
		}
	}
	t.Fatal("missing completion")
}
