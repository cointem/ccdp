package execution

import (
	"fmt"
	"strings"
)

// ReadOnlyGitArgv returns an argv for a git query which is insulated from
// repository configuration that can otherwise run a helper or a pager.  The
// caller must have already admitted the command as read-only; this helper only
// adds the execution hardening that applies to that admitted class.
//
// Keep the options before the subcommand so git treats them as global options,
// and put diff options after the subcommand.  The latter covers `diff`, `log`
// and `show` even when their repository configuration requests text conversion
// or an external diff implementation.
func ReadOnlyGitArgv(argv []string) ([]string, error) {
	if len(argv) < 2 || argv[0] != "git" {
		return nil, fmt.Errorf("execution: read-only git argv must start with git and a subcommand")
	}

	out := make([]string, 0, len(argv)+12)
	out = append(out, "git",
		"-c", "core.pager=cat",
		"-c", "pager.diff=false",
		"-c", "pager.log=false",
		"-c", "diff.external=",
		"-c", "core.fsmonitor=false",
		argv[1])
	if argv[1] == "diff" || argv[1] == "log" || argv[1] == "show" {
		out = append(out, "--no-ext-diff", "--no-textconv")
	}
	out = append(out, argv[2:]...)
	return out, nil
}

// ReadOnlyGitEnvironment removes caller/host overrides for the Git helpers
// controlled at this boundary.  Values are replaced rather than appended to
// avoid duplicate environment keys with platform-dependent precedence.
func ReadOnlyGitEnvironment(entries []string) []string {
	base := SanitizedEnvironmentFor(EnvironmentGit, entries)
	const (
		pager         = "GIT_PAGER"
		plainPager    = "PAGER"
		externalDiff  = "GIT_EXTERNAL_DIFF"
		optionalLocks = "GIT_OPTIONAL_LOCKS"
		noSystem      = "GIT_CONFIG_NOSYSTEM"
	)
	filtered := make([]string, 0, len(base)+5)
	for _, entry := range base {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		switch key {
		case pager, plainPager, externalDiff, optionalLocks, noSystem:
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered,
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_EXTERNAL_DIFF=",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
}
