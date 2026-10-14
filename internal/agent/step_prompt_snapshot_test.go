package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/workspace"
)

func TestPreparedStepRequestDoesNotReloadMutablePromptInputs(t *testing.T) {
	a := newRuntimeAgent(t)
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "initial query"}); err != nil {
		t.Fatal(err)
	}
	step, err := a.beginStepChecked()
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseStepContext(step)
	before := a.buildRequestForStep(step)
	if err := os.WriteFile(filepath.Join(step.cfg.Workspace, "AFTER_STEP_MARKER.go"), []byte("package changed"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace.Invalidate(step.cfg.Workspace)
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "changed query"}); err != nil {
		t.Fatal(err)
	}
	after := a.buildRequestForStep(step)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("request rebuilt prompt from live inputs after step preparation")
	}
}
