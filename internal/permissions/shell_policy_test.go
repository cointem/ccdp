package permissions

import "testing"

func TestReadOnlyArgumentPolicies(t *testing.T) {
	for _, command := range []string{
		"xxd -r input.hex output.txt", "xxd input output", "xxd -ps input output",
		"rg --pre=helper pattern file", "rg --no-config --pre helper pattern file",
		"rg --no-config --pre=helper pattern file", "rg --no-config --pre-glob '*.txt' pattern file",
		"rg pattern file", // inherited RIPGREP_CONFIG_PATH can inject --pre
		"rg --no-config -z pattern file", "file -C", "file -z archive.gz",
		"base64 -i -- -o output", "base64 -di -- -o output", "base64 -b -- -o output", "base64 -w -- -o output",
		"base64 -o output input", "base64 --output=output input", "less input",
		"find . -execdir helper '{}' +", "find . -fprint output", "find . -- -delete",
		"ls --unknown", "ls -lZ", "env PAGER=helper ls", "env -S 'touch marker'",
		"date 1010101010", "date --set yesterday", "date -s yesterday",
		"git status", "git log --output=out", "git show --ext-diff",
		"git diff --no-ext-diff --no-textconv", "git grep --open-files-in-pager=helper pattern",
	} {
		t.Run(command, func(t *testing.T) {
			m := NewManager(ModeDefault, Policy{})
			if d, why := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionAsk {
				t.Fatalf("got %s (%s), want ask", d, why)
			}
		})
	}
	for _, command := range []string{
		"ls -lah", "cat -n -- file", "head -n20 file", "tail -n 20 file",
		"rg --no-config -n -g '*.go' -- pattern .", "rg --no-config -A2 --fixed-strings pattern file",
		"grep -nE -- 'a|b' file", "find . -type f -name '*.go' -print",
		"xxd -l 16 input", "xxd -g1 input", "xxd -- input", "base64 -d input",
		"env ls -l", "env env cat file", "env", "date -u '+%Y-%m-%d'",
		"echo hello | cat", "ls && cat file", "ls; pwd", "echo 'a; b > c $(literal)'",
	} {
		t.Run(command, func(t *testing.T) {
			m := NewManager(ModeDefault, Policy{})
			if d, why := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionAllow {
				t.Fatalf("got %s (%s), want allow", d, why)
			}
		})
	}
}

func TestRuleMatchingUsesTheSameShellBoundary(t *testing.T) {
	m := NewManager(ModeDefault, Policy{AlwaysAllow: []string{"Bash:echo*", "Bash:cat*", "Bash:ls*"}})
	for _, command := range []string{
		`echo "$(touch marker)"`, "echo \"`touch marker`\"", `echo ${x:-$(touch marker)}`,
		`echo "$((1 + $(touch marker)))"`, `echo "a\q$(touch marker)"`,
		`echo hello > marker`, `echo hello 2> marker`, `cat < input`,
		"cat <<EOF\n$(touch marker)\nEOF", `cat <(touch marker)`,
		`echo ok; touch marker`, `echo ok | sh`, `echo $WORDS`, `ls *`,
		`echo [ab]`, `echo ?`, `echo "unfinished`, "echo trailing\\",
	} {
		t.Run(command, func(t *testing.T) {
			if d, why := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionAsk {
				t.Fatalf("got %s (%s), want ask", d, why)
			}
		})
	}
	for _, command := range []string{`echo '$(touch marker)'`, "echo '`touch marker`'", `echo "\$(literal)"`, `echo '*'`, `echo a\;b`, "echo ok && cat file", `echo "a\q"`} {
		if d, why := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionAllow {
			t.Errorf("%q: got %s (%s)", command, d, why)
		}
	}
	// Explicit full-command consent and bypass remain available for shell work
	// outside the conservative grammar. The analysis does not ban shell syntax.
	command := `echo "$(date)"`
	m.RememberAllow(CommandKey(command))
	if d, _ := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionAllow {
		t.Fatal("exact session approval lost")
	}
}

func TestLiteralShellWords(t *testing.T) {
	segments, ok := analyzeShell(`echo "a\q" 'b c' d\ e '' "\$value"`)
	if !ok || len(segments) != 1 {
		t.Fatalf("parse = %v, %v", segments, ok)
	}
	want := []string{"echo", `a\q`, "b c", "d e", "", "$value"}
	got := segments[0].words
	if len(got) != len(want) {
		t.Fatalf("argv = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d: %q != %q", i, got[i], want[i])
		}
	}
}

func TestTypedGitArgumentPolicies(t *testing.T) {
	for _, argv := range [][]string{
		{"git", "status", "--short"}, {"git", "log", "--oneline", "-n5"},
		{"git", "diff", "--no-ext-diff", "--no-textconv", "--", "a b.txt"},
		{"git", "diff", "--cached"}, {"git", "remote", "get-url", "origin"},
	} {
		if !ReadOnlyGitArguments(argv) {
			t.Errorf("query argv rejected: %q", argv)
		}
	}
	for _, argv := range [][]string{
		{"git", "log", "--output=out"}, {"git", "log", "--out", "out"},
		{"git", "show", "--textconv"}, {"git", "diff", "--ext-diff"},
		{"git", "diff", "--format", "--", "--output=out"},
		{"git", "-c", "core.pager=helper", "status"}, {"git", "branch", "--edit-description"},
		{"git", "branch", "--delete=main"}, {"git", "tag", "v1"},
		{"git", "remote", "show", "origin"}, {"git", "grep", "--open-files-in-pager=helper", "pattern"},
	} {
		if ReadOnlyGitArguments(argv) {
			t.Errorf("unsafe argv accepted: %q", argv)
		}
	}
}

func TestStaticShellQuotingCannotBypassHardDenials(t *testing.T) {
	for _, command := range []string{`'git' 'push' '-f' 'origin'`, `g"it" pu'sh' --force origin`, `echo ok && 'git' 'push' '-f' origin`} {
		m := NewManager(ModeBypass, Policy{})
		if d, _ := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionDeny {
			t.Errorf("force push escaped deny: %q", command)
		}
	}
	for _, command := range []string{`'git' 'push' 'origin'`, `g"it" pu'sh' origin`, `echo ok; 'git' 'push' origin`} {
		m := NewManager(ModeBypass, Policy{AlwaysDeny: []string{"Bash:git push*"}})
		if d, _ := m.Check("Bash", map[string]any{"command": command}, KindShell); d != DecisionDeny {
			t.Errorf("rule escaped deny: %q", command)
		}
	}
}
