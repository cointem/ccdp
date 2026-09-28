package session

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSnapshotStoresAgreeOnSchemaVersion(t *testing.T) {
	disk, err := OpenJSONLStore(t.TempDir(), "schema")
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	memory := NewMemoryStore()
	for _, version := range []uint32{0, 1, SchemaVersion, SchemaVersion + 1} {
		snap := Snapshot{SchemaVersion: version, SessionID: "schema", Data: json.RawMessage(`{}`)}
		a, b := disk.SaveSnapshot(snap), memory.SaveSnapshot(snap)
		if (a == nil) != (b == nil) || errors.Is(a, ErrFutureVersion) != errors.Is(b, ErrFutureVersion) {
			t.Fatalf("v%d: disk=%v memory=%v", version, a, b)
		}
		if version == 1 && a == nil {
			t.Fatal("legacy version accepted")
		}
	}
}
