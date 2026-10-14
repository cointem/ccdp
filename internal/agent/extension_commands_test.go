package agent

import (
	"context"
	"errors"
	"testing"

	"ccdp/internal/permissions"
)

func TestExternalCommandReadOnlyChecksCompleteArgv(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"git", "status", "--short"}, true},
		{[]string{"git", "branch", "--show-current"}, true},
		{[]string{"git", "branch", "-D", "main"}, false},
		{[]string{"git", "branch", "--move", "old", "new"}, false},
		{[]string{"git", "diff", "--output=/tmp/result"}, false},
		{[]string{"git", "remote", "get-url", "origin"}, true},
		{[]string{"gh", "pr", "view", "--web"}, false},
		{[]string{"gh", "pr", "view", "--json", "url"}, true},
		{[]string{"gh", "pr", "merge", "123"}, false},
	}
	for _, tc := range cases {
		if got := externalCommandReadOnly(tc.argv); got != tc.want {
			t.Errorf("externalCommandReadOnly(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestExternalCommandFailureExplainsGitStatus(t *testing.T) {
	if got := externalCommandFailure("git", 129,
		"warning: Not a git repository. Use --no-index to compare two paths outside a working tree\nusage: git diff --no-index").Error(); got != "git failed: workspace is not a git repository" {
		t.Fatalf("not-a-repository message = %q", got)
	}
	if got := externalCommandFailure("git", 128, "fatal: bad revision").Error(); got != "git exited with status 128: fatal: bad revision" {
		t.Fatalf("reason passthrough = %q", got)
	}
	if got := externalCommandFailure("gh", 1, "").Error(); got != "gh exited with status 1" {
		t.Fatalf("empty-output message = %q", got)
	}
}

func TestTypedGitAuthorizationPreservesDenials(t *testing.T) {
	a := &Agent{services: sessionServices{perms: permissions.NewManager(permissions.ModeDefault, permissions.Policy{})}}
	argv := []string{"git", "status", "--short"}
	if err := a.authorizeGitQuery(context.Background(), "query", argv); err != nil {
		t.Fatal(err)
	}
	a.services.perms.SetPolicy(permissions.Policy{AlwaysDeny: []string{"Bash:git status*"}})
	if err := a.authorizeGitQuery(context.Background(), "query", argv); err == nil {
		t.Fatal("typed query bypassed deny")
	}
	a.services.perms.SetPolicy(permissions.Policy{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.authorizeGitQuery(ctx, "query", argv); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: %v", err)
	}
}

func TestTypedMutatingGitCannotQuoteAwayHardDenials(t *testing.T) {
	a := &Agent{services: sessionServices{perms: permissions.NewManager(permissions.ModeBypass, permissions.Policy{})}}
	force := formatExternalCommand([]string{"git", "push", "-f", "origin"})
	if err := a.precheckCommand("Bash", map[string]any{"command": force}, permissions.KindShell); err == nil {
		t.Fatal("quoted force push bypassed hard deny")
	}
	a.services.perms.SetPolicy(permissions.Policy{AlwaysDeny: []string{"Bash:git push*"}})
	push := formatExternalCommand([]string{"git", "push", "origin"})
	if err := a.authorizeCommand(context.Background(), "push", "Bash", map[string]any{"command": push}, permissions.KindShell); err == nil {
		t.Fatal("quoted push bypassed configured deny")
	}
}
