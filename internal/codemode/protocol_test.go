package codemode

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWorkerFramesPreserveExactJSONNumbers(t *testing.T) {
	var wire bytes.Buffer
	want := json.Number("9007199254740993")
	if err := writeFrame(&wire, frame{Type: "call", Args: map[string]any{"id": want}}); err != nil {
		t.Fatal(err)
	}
	got, err := readFrame(&wire)
	if err != nil || got.Args["id"] != want {
		t.Fatalf("number changed across worker transport: %#v %v", got.Args, err)
	}
}

func TestProgramDataRejectsNumbersThatJavaScriptWouldRound(t *testing.T) {
	for _, raw := range []string{`{"id":9007199254740993}`, `[1e1000]`, `{"id":-9007199254740993}`} {
		if err := ValidateData(json.RawMessage(raw)); err == nil {
			t.Fatalf("would silently round %s", raw)
		}
	}
	for _, raw := range []string{`{"id":9007199254740991}`, `[12,0.25,null]`, `{"id":"9007199254740993"}`} {
		if err := ValidateData(json.RawMessage(raw)); err != nil {
			t.Fatalf("valid program data rejected: %s: %v", raw, err)
		}
	}
}
