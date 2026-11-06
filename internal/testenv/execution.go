// Package testenv constructs the same executor boundary used by application
// sessions. Integration tests must not rely on a host execution bypass.
package testenv

import (
	"context"
	"testing"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// Prepare excludes one-time executor/profile setup from execution latency tests.
func Prepare(t testing.TB, policy *sandbox.Sandbox) {
	t.Helper()
	result, err := execution.Run(execution.Request{Context: context.Background(), Command: "true", Dir: policy.Workspace, Sandbox: policy})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("prepare test execution environment: %+v %v", result, err)
	}
}

func Sandbox(t testing.TB, workspace string) *sandbox.Sandbox {
	t.Helper()
	policy := sandbox.New(workspace)
	policy.SetScratchDir(t.TempDir())
	controller := execution.NewController(t.TempDir() + "/runtime")
	policy.ExecutorID = controller.ID
	t.Cleanup(func() {
		if err := controller.Close(); err != nil {
			t.Errorf("close test executor: %v", err)
		}
	})
	return policy
}
