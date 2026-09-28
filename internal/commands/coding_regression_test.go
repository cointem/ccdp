package commands

import (
	"strings"
	"testing"
)

func TestLocalFlagIsDeliveryOnly(t *testing.T) {
	for _, name := range []string{"review", "rewind", "merge", "index"} {
		if _, err := ParseCodingWorkflow(name, []string{"--local"}); err == nil || !strings.Contains(err.Error(), "only supported by delivery") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	w, err := ParseCodingWorkflow("deliver", []string{"message", "--local", "--all"})
	if err != nil || w.Mode != "commit" {
		t.Fatal(w, err)
	}
}
