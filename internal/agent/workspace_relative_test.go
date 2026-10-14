package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/protocol"
)

func TestSetWorkspaceRelativeToCurrentWorkspace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "review-relative-child")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	a := newGithubAgent(t, dir)
	defer a.Close()
	cmd := protocol.Command{ID: "review-relative-cd", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetWorkspace, Workspace: &protocol.WorkspaceCommand{Path: "review-relative-child"}}
	receipt, err := a.Submit(context.Background(), cmd)
	if err != nil || receipt.Rejected() {
		t.Fatalf("submit: %+v %v", receipt, err)
	}
	final := waitFormalCommandReceipt(t, a, cmd.ID)
	if final.Rejected() {
		t.Fatalf("relative cd resolved outside current workspace: %+v", final.Error)
	}
	if got := a.configSnapshot().Workspace; got != target {
		t.Fatalf("workspace=%s want=%s", got, target)
	}
}
