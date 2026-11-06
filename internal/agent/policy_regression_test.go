package agent

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/session/archive"
)

func TestPolicyCommandsPersistCandidateValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, kind := range []string{"permission", "sandbox"} {
		t.Run(kind, func(t *testing.T) {
			a := newRuntimeAgent(t)
			cmd := protocol.Command{Type: protocol.CommandSetPermissionPolicy,
				PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{
					Mode: "manual", AlwaysAllow: []string{"Read:*"}, AlwaysDeny: []string{"Bash:rm *"},
				}}}
			if kind == "sandbox" {
				cmd = protocol.Command{Type: protocol.CommandSetSandboxPolicy,
					SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{
						NetworkAccess:         !a.services.cfg.NetworkAccess,
						AdditionalDirectories: []string{filepath.Join(a.services.cfg.Workspace, "extra")},
						DisallowedDirectories: []string{filepath.Join(a.services.cfg.Workspace, "blocked")},
					}}}
			}
			r, err := a.Submit(context.Background(), cmd)
			if err != nil || r.Rejected() {
				t.Fatalf("policy: receipt=%+v err=%v", r, err)
			}
			a.mu.Lock()
			want := a.sessionSettingsLocked()
			a.mu.Unlock()
			saved, err := archive.Load(a.SessionDir(), a.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			if saved.Settings == nil || !reflect.DeepEqual(*saved.Settings, want) {
				t.Fatalf("persisted policy does not match applied policy:\ngot  %+v\nwant %+v", saved.Settings, want)
			}
		})
	}
}

func TestGuardedCommandStillRejectsStaleRevision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newRuntimeAgent(t)
	before, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision.LogSeq == 0 {
		t.Fatal("expected nonzero initial revision")
	}
	r, err := a.Submit(context.Background(), protocol.Command{Type: protocol.CommandSetPermissionPolicy,
		ExpectedRevision: before.Revision.LogSeq,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: "manual"}}})
	if err != nil || r.Rejected() {
		t.Fatalf("fresh command: receipt=%+v err=%v", r, err)
	}
	r, err = a.Submit(context.Background(), protocol.Command{Type: protocol.CommandSetPermissionPolicy,
		ExpectedRevision: before.Revision.LogSeq,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: "auto"}}})
	if err != nil || r.Error == nil || r.Error.Code != protocol.ErrorStaleRevision {
		t.Fatalf("stale command: receipt=%+v err=%v", r, err)
	}
	after, err := a.Snapshot(context.Background())
	if err != nil || after.Settings.Permission.Mode != "manual" {
		t.Fatalf("stale command changed permission mode: %+v err=%v", after.Settings, err)
	}
}
