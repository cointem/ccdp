package session

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

func TestUnknownEventRejectsWholeTransactionWithoutChangingLog(t *testing.T) {
	root := t.TempDir()
	store, err := OpenJSONLStore(root, "unsupported")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(Beginning, Batch{TransactionID: "known", Events: []Event{testInput("known", "keep")}}); err != nil {
		t.Fatal(err)
	}
	path := store.EventsPath()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeEvent(testInput("after", "must not appear"))
	if err != nil {
		t.Fatal(err)
	}
	line, _, err := encodeTransaction("unsupported", 2, []wireEvent{{Type: "UnsupportedEvent", Payload: []byte(`{"data":true}`)}, wire}, "test-input")
	if err != nil {
		t.Fatal(err)
	}
	log := append(append([]byte(nil), before...), append(line, '\n')...)
	if err := os.WriteFile(path, log, 0600); err != nil {
		t.Fatal(err)
	}
	for _, readOnly := range []bool{true, false} {
		var opened *JSONLStore
		if readOnly {
			opened, err = OpenJSONLReadOnly(root, "unsupported")
		} else {
			opened, err = OpenJSONLStore(root, "unsupported")
		}
		if opened != nil {
			opened.Close()
		}
		if !errors.Is(err, ErrUnknownEventType) {
			t.Fatalf("readOnly=%v error=%v", readOnly, err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, log) {
			t.Fatal("unsupported event caused log rewriting")
		}
	}
}
