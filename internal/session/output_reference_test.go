package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ccdp/internal/messages"
)

func TestToolOutputReferenceRoundTripKeepsTypedStatus(t *testing.T) {
	for _, status := range []string{"success", "error", "denied", "unknown"} {
		input := messages.Message{ID: "result", Role: messages.RoleTool, ToolCallID: "call", ToolStatus: status,
			Content: "Error: this is literal output, not the execution status", DisplayTruncated: true, DataError: "bad JSON",
			OutputRef:     &messages.OutputReference{Hash: strings.Repeat("a", 64), Size: 8192, MediaType: "text/plain"},
			SourceOutputs: []messages.OutputReference{{Hash: strings.Repeat("b", 64), Size: 50}}}
		stored, err := EncodeMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeMessage(stored)
		if err != nil || !reflect.DeepEqual(got, input) {
			t.Fatalf("round trip %s: %+v, %v", status, got, err)
		}
		cloned := messages.Clone([]messages.Message{got})
		cloned[0].OutputRef.Hash = "changed"
		cloned[0].SourceOutputs[0].Hash = "changed"
		if input.OutputRef.Hash != got.OutputRef.Hash || input.SourceOutputs[0] != got.SourceOutputs[0] {
			t.Fatal("output reference aliases clone")
		}
	}
	stored, err := EncodeMessage(messages.Message{Role: messages.RoleTool, ToolCallID: "untyped", Content: "Error: text"})
	if err != nil || stored.Content[0].ToolResult.Status != "unknown" {
		t.Fatal("missing status inferred from text")
	}
}

func TestOldSchemaIsRejectedWithoutChangingLog(t *testing.T) {
	root := t.TempDir()
	store, err := OpenJSONLStore(root, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(Beginning, NewBatch(testInput("input", "keep this data"))); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "old", eventsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(data, []byte(fmt.Sprintf(`"schema_version":%d`, SchemaVersion)), []byte(fmt.Sprintf(`"schema_version":%d`, SchemaVersion-1)), 1)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONLStore(root, "old"); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("old schema should be unsupported: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, old) {
		t.Fatal("old session was rewritten")
	}
}
