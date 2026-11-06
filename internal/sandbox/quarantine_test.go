package sandbox

import (
	"path/filepath"
	"testing"
)

func TestQuarantineRetainsOnceRootsAcrossRestart(t *testing.T) {
	workspace, once, later, independent := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	base := New(workspace)
	base.QuarantineStore = filepath.Join(t.TempDir(), "quarantine.json")
	invocation := base.Snapshot()
	invocation.AdditionalDirs = []string{once}
	for i := 0; i < 1000; i++ {
		invocation.MarkExternalExecution()
	}
	if len(base.executionGuard.scopes) != 1 || len(base.executionGuard.scopes[0]) != 2 {
		t.Fatal("execution witnesses grew per command")
	}
	if err := base.RecordQuarantine("unconfirmed descendants"); err != nil {
		t.Fatal(err)
	}
	invocation.AdditionalDirs = append(invocation.AdditionalDirs, later)
	invocation.MarkExternalExecution()
	if err := base.RecordQuarantine("broader uncertainty"); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{workspace, once, later} {
		restarted := New(root)
		restarted.QuarantineStore = base.QuarantineStore
		if err := restarted.CheckExecution(); err == nil {
			t.Fatalf("restart lost quarantine for %s", root)
		}
	}
	other := New(independent)
	other.QuarantineStore = base.QuarantineStore
	other.ShareExecutionGuard(base)
	if other.ExternalExecutionPossible() {
		t.Fatal("witness leaked into independent workspace")
	}
	if err := other.CheckExecution(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivacyTighteningRequiresNewAdmission(t *testing.T) {
	base := New(t.TempDir())
	for _, change := range []func(*Sandbox){
		func(s *Sandbox) { s.CredentialDirs = []string{t.TempDir()} },
		func(s *Sandbox) { s.AddPrivateRoot(t.TempDir()) },
		func(s *Sandbox) { s.ArtifactRoots = nil },
	} {
		base.ArtifactRoots = []string{t.TempDir()}
		next := base.Snapshot()
		change(next)
		if !PolicyTightened(base, next) {
			t.Fatal("privacy tightening did not retire old authority")
		}
	}
}
