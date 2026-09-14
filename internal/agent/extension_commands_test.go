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
