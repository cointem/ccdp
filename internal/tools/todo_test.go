package tools

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTodoWriteUsesSessionWriter(t *testing.T) {
	ctx := freshCtx(t)
	var committed []TodoState
	ctx.ReplaceTodos = func(states []TodoState) error {
		committed = append([]TodoState(nil), states...)
		return nil
	}
	out, err := runTool(t, NewTodoWriteTool(), ctx, map[string]any{"todos": []any{
		map[string]any{"content": "step one", "status": "in_progress", "priority": "high"},
		map[string]any{"content": "step two"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []TodoState{{ID: "1", Content: "step one", Status: "in_progress", Priority: "high"}, {ID: "2", Content: "step two", Status: "pending", Priority: "medium"}}
	if !reflect.DeepEqual(committed, want) {
		t.Fatalf("committed=%+v", committed)
	}
	if !strings.Contains(out, "(high) step one") || !strings.Contains(TodoSection(committed), "[~] (high) step one") {
		t.Fatal(out, TodoSection(committed))
	}
	if _, err = runTool(t, NewTodoWriteTool(), ctx, map[string]any{"todos": []any{}}); err != nil {
		t.Fatal(err)
	}
	if len(committed) != 0 {
		t.Fatalf("empty replacement kept tasks: %+v", committed)
	}
}

func TestTodoWriteRequiresSuccessfulCommit(t *testing.T) {
	ctx := freshCtx(t)
	args := map[string]any{"todos": []any{map[string]any{"content": "new"}}}
	if _, err := runTool(t, NewTodoWriteTool(), ctx, args); err == nil {
		t.Fatal("missing session writer accepted")
	}
	failure := errors.New("commit failed")
	calls := 0
	ctx.ReplaceTodos = func([]TodoState) error { calls++; return failure }
	if out, err := runTool(t, NewTodoWriteTool(), ctx, args); !errors.Is(err, failure) || out != "" {
		t.Fatalf("failed commit reported success: %q %v", out, err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if _, err := runTool(t, NewTodoWriteTool(), ctx, map[string]any{"todos": []any{map[string]any{"content": "new", "priority": "invalid"}}}); err == nil {
		t.Fatal("invalid priority accepted")
	}
	if calls != 1 {
		t.Fatal("invalid replacement reached owner")
	}
	if err := ctx.Resources.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, NewTodoWriteTool(), ctx, args); err == nil {
		t.Fatal("closed session accepted tasks")
	}
	if calls != 1 {
		t.Fatal("closed resource reached owner")
	}
}

func TestTodoSectionOldRecordPriority(t *testing.T) {
	if got := TodoSection([]TodoState{{ID: "1", Content: "legacy", Status: "pending"}}); !strings.Contains(got, "(medium) legacy") {
		t.Fatal(got)
	}
}
