package agent

import "testing"

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
