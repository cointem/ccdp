package permissions

import (
	"ccdp/internal/executable"
	"os"
	"strings"
)

// argvPolicy lists supported flags, including their arity. Unknown flags ask
// rather than assuming a nominally read-only program has no write/exec mode.
// Exact long names avoid GNU option abbreviation; short bundles are parsed one
// flag at a time, and a value-taking flag consumes the rest of its bundle.
type argvPolicy struct {
	flags          map[string]bool // true means one required argument
	positionals    bool
	maxPositionals int // zero means unlimited
}

func flags(positionals bool, switches, values string) argvPolicy {
	p := argvPolicy{flags: map[string]bool{}, positionals: positionals}
	for _, f := range strings.Fields(switches) {
		p.flags[f] = false
	}
	for _, f := range strings.Fields(values) {
		p.flags[f] = true
	}
	return p
}

func (p argvPolicy) allows(args []string) bool {
	literal := false
	positionalCount := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if literal || arg == "-" || !strings.HasPrefix(arg, "-") {
			if !p.positionals {
				return false
			}
			positionalCount++
			if p.maxPositionals > 0 && positionalCount > p.maxPositionals {
				return false
			}
			continue
		}
		if arg == "--" {
			literal = true
			continue
		}
		// Enumerated optional values are separate entries; unlike a generic
		// optional-argument parser this cannot consume a following -- sentinel.
		if takes, known := p.flags[arg]; known && !takes {
			continue
		}
		name, value, equal := strings.Cut(arg, "=")
		takes, known := p.flags[name]
		if known {
			if equal {
				if !takes || value == "" || value == "--" {
					return false
				}
				continue
			}
			if takes {
				i++
				if i == len(args) || args[i] == "--" {
					return false
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "--") {
			return false
		}
		// A short option bundle is accepted only if every option is known.
		for j := 1; j < len(arg); j++ {
			takes, known = p.flags["-"+arg[j:j+1]]
			if !known {
				return false
			}
			if takes {
				if j+1 == len(arg) {
					i++
					if i == len(args) || args[i] == "--" {
						return false
					}
				}
				break
			}
		}
	}
	return true
}

var readOnlyCommands = map[string]argvPolicy{
	"ls":       flags(true, "-a -A -l -h -t -r -S -d -F -p -R -i -n -s -1 -L -H -P -G -C -x -q -b -U -u -c --all --almost-all --human-readable --recursive --directory --numeric-uid-gid --classify --help --version", "--color --sort --time --time-style --format --indicator-style --quoting-style --block-size --ignore --hide"),
	"pwd":      flags(false, "-L -P --help --version", ""),
	"cat":      flags(true, "-A -b -e -E -n -s -t -T -u -v --show-all --number --number-nonblank --squeeze-blank --show-ends --show-tabs --show-nonprinting --help --version", ""),
	"head":     flags(true, "-q -v --quiet --silent --verbose --help --version", "-n -c --lines --bytes"),
	"tail":     flags(true, "-q -v -f -F --quiet --silent --verbose --retry --help --version", "-n -c -s --lines --bytes --sleep-interval --pid --max-unchanged-stats"),
	"wc":       flags(true, "-c -m -l -w -L --bytes --chars --lines --words --max-line-length --help --version", "--files0-from"),
	"grep":     flags(true, "-E -F -G -P -i -v -w -x -c -l -L -n -H -h -r -R -a -I -s -q -o -b -z -Z --extended-regexp --fixed-strings --ignore-case --invert-match --word-regexp --line-regexp --count --files-with-matches --files-without-match --line-number --with-filename --no-filename --recursive --text --quiet --only-matching --null-data --null --help --version", "-e -f -m -A -B -C --regexp --file --max-count --after-context --before-context --context --include --exclude --exclude-dir --binary-files --color"),
	"rg":       flags(true, "-i -s -S -v -w -x -F -P -n -N -H -I -l -L -c -o -a -0 -U -u -q --hidden --no-ignore --no-ignore-vcs --no-require-git --files --count --count-matches --files-with-matches --files-without-match --line-number --no-line-number --with-filename --no-filename --heading --no-heading --json --only-matching --fixed-strings --pcre2 --multiline --multiline-dotall --follow --text --null --null-data --stats --no-messages --no-config --help --version", "-e -f -g -t -T -m -A -B -C -j -M --regexp --file --glob --iglob --type --type-not --max-count --after-context --before-context --context --threads --max-columns --max-depth --max-filesize --color --sort --sortr --encoding --replace --path-separator --ignore-file --type-add"),
	"which":    flags(true, "-a -s --all --help --version", ""),
	"whoami":   flags(false, "--help --version", ""),
	"printenv": flags(true, "-0 --null --help --version", ""),
	"du":       flags(true, "-a -s -h -k -m -c -x -L -H -P --all --summarize --human-readable --total --one-file-system --help --version", "-d --max-depth --block-size --exclude"),
	"df":       flags(true, "-a -h -H -k -m -i -l -P -T --all --human-readable --inodes --local --portability --print-type --help --version", "-t -x --type --exclude-type --block-size"),
	"uname":    flags(false, "-a -s -n -r -v -m -p -i -o --all --kernel-name --nodename --kernel-release --kernel-version --machine --processor --hardware-platform --operating-system --help --version", ""),
	"uptime":   flags(false, "-p -s -V --pretty --since --help --version", ""),
	"date":     flags(true, "-u -R -I --utc --universal --rfc-email --help --version", "-d -r --date --reference"),
	"base64":   flags(true, "-d -D --decode --ignore-garbage --help --version", "-b -w --wrap"),
	"xxd":      flags(true, "-a -b -e -E -u -p -ps -i", "-g -c -l -s"),
	"hexdump":  flags(true, "-b -c -C -d -o -x -v", "-e -f -n -s"),
	"file":     flags(true, "-b -c -i -I -k -L -N -n -r -s --brief --mime --mime-type --mime-encoding --keep-going --dereference --no-buffer --raw --special-files --help --version", "-e -f -F -m --exclude --files-from --separator --magic-file"),
	"stat":     flags(true, "-L -f -t -x --dereference --file-system --terse --help --version", "-c --format --printf"),
	"nl":       flags(true, "--help --version", "-b -d -f -h -i -l -n -s -v -w --body-numbering --section-delimiter --footer-numbering --header-numbering --line-increment --join-blank-lines --number-format --number-separator --starting-line-number --number-width"),
	"diff":     flags(true, "-a -b -B -c -e -i -n -N -q -r -s -t -T -u -w -y --text --ignore-space-change --ignore-blank-lines --ignore-case --new-file --brief --recursive --report-identical-files --expand-tabs --initial-tab --ignore-all-space --side-by-side --help --version", "-C -U -W -I -x -X --context --unified --width --ignore-matching-lines --exclude --exclude-from --label"),
}

func readOnlyArgv(words []string) bool {
	if len(words) == 0 {
		return false
	}
	// env with literal argv and no assignments/options is a transparent wrapper.
	// Environment changes can enable execution helpers (PAGER, GIT_CONFIG, etc.).
	external := false
	for words[0] == "env" {
		if !trustedReadOnlyCommand("env") {
			return false
		}
		external = true
		if len(words) == 1 {
			return true
		}
		words = words[1:]
	}
	if !external {
		switch words[0] {
		case "echo", "true", "false", "pwd":
			if words[0] != "pwd" || readOnlyCommands["pwd"].allows(words[1:]) {
				return true
			}
		}
	}
	if !trustedReadOnlyCommand(words[0]) {
		return false
	}
	switch words[0] {
	case "echo", "true", "false":
		return true
	case "sleep":
		return flags(true, "--help --version", "").allows(words[1:])
	case "git":
		// Raw shell Git can execute repository-configured pagers, fsmonitor and
		// text conversion even with query-only arguments. Only the typed adapter
		// can guarantee both argv and environment hardening.
		return false
	case "find":
		return findReadOnly(words[1:])
	case "rg":
		// RIPGREP_CONFIG_PATH is inherited by ordinary shell commands and can
		// inject --pre. Require explicit config isolation before any other args.
		return len(words) > 1 && words[1] == "--no-config" && readOnlyCommands["rg"].allows(words[1:])
	case "xxd":
		p := readOnlyCommands["xxd"]
		p.maxPositionals = 1
		return p.allows(words[1:])
	case "date":
		// A bare date operand sets the system clock on BSD/macOS.
		for _, arg := range words[1:] {
			if arg != "" && arg[0] >= '0' && arg[0] <= '9' {
				return false
			}
		}
	}
	p, ok := readOnlyCommands[words[0]]
	return ok && p.allows(words[1:])
}

func findReadOnly(args []string) bool {
	// find has primary expressions, not POSIX option parsing. An explicit
	// positive set avoids mutation aliases and future execution primaries.
	p := flags(true, "-H -L -P -E -X -d -s -x -print -print0 -ls -empty -readable -writable -executable -nouser -nogroup -prune -quit -depth -mount -xdev -true -false -a -and -o -or -not", "-name -iname -path -ipath -regex -iregex -type -size -mtime -mmin -atime -amin -ctime -cmin -newer -user -group -perm -links -inum -maxdepth -mindepth")
	// find does not treat -- as a portable end of primary processing.
	for _, arg := range args {
		if arg == "--" {
			return false
		}
	}
	return p.allows(args)
}

// ReadOnlyGitArguments checks only the explicit arguments. It is for typed
// adapters that subsequently use execution.RunReadOnlyGit or StartReadOnlyGit.
// It must never authorize raw shell Git.
func ReadOnlyGitArguments(words []string) bool {
	if len(words) < 2 || words[0] != "git" {
		return false
	}
	args := words[2:]
	switch words[1] {
	case "status":
		return flags(true, "-s -b -z --short --branch --show-stash --long --verbose --porcelain --porcelain=v1 --porcelain=v2 --no-renames --renames --ahead-behind -u --untracked-files --untracked-files=all --untracked-files=normal --untracked-files=no --ignored --ignored=traditional --ignored=matching --ignored=no", "--ignore-submodules --column --find-renames").allows(args)
	case "log", "show", "diff":
		p := flags(true, "-p -u -s -U -w -b -B -M -C --patch --no-patch --stat --shortstat --numstat --name-only --name-status --oneline --graph --all --branches --tags --remotes --decorate --no-decorate --reverse --no-merges --merges --first-parent --no-ext-diff --no-textconv --no-color --color --color=never --color=always --color=auto --check --exit-code --quiet --cached --staged --binary --full-index --no-renames --relative --ignore-space-at-eol --ignore-blank-lines --no-prefix", "-n --max-count --format --pretty --date --since --until --after --before --author --committer --grep --encoding --diff-filter --src-prefix --dst-prefix")
		if !p.allows(args) {
			return false
		}

		return true
	case "branch":
		return flags(false, "-a -r -v -q --all --remotes --verbose --quiet --list --show-current --no-color", "--sort --format --contains --no-contains --merged --no-merged --points-at").allows(args)
	case "tag":
		return flags(false, "-l -n --list --column --no-column", "--sort --format --contains --no-contains --merged --no-merged --points-at").allows(args)
	case "remote":
		if len(args) == 0 {
			return true
		}
		if args[0] == "get-url" {
			return flags(true, "--all --push", "").allows(args[1:])
		}
		// remote show contacts a remote unless -n is supplied.
		if args[0] == "show" {
			return len(args) > 1 && args[1] == "-n" && flags(true, "-n", "").allows(args[1:])
		}
		return flags(false, "-v --verbose", "").allows(args)
	case "config":
		return len(args) == 1 && (args[0] == "--list" || args[0] == "-l")
	case "stash":
		return len(args) == 1 && args[0] == "list"
	case "ls-files":
		return flags(true, "-z -t -v -f -c -d -m -o -i -s -u -k --cached --deleted --modified --others --ignored --stage --unmerged --killed --directory --no-empty-directory --exclude-standard --error-unmatch --full-name --recurse-submodules --deduplicate", "-x -X --exclude --exclude-from --exclude-per-directory --format").allows(args)
	case "rev-parse":
		return flags(true, "--verify --quiet --short --abbrev-ref --symbolic --symbolic-full-name --show-toplevel --show-prefix --show-cdup --git-dir --git-common-dir --is-inside-work-tree --is-bare-repository --show-object-format --sq --all --branches --tags --remotes", "").allows(args)
	case "describe":
		return flags(true, "--all --tags --contains --always --long --exact-match --first-parent", "--abbrev --candidates --match --exclude").allows(args)
	case "blame":
		return flags(true, "-p -c -s -e -w --porcelain --line-porcelain --incremental --root --show-email --show-name --show-number --no-progress --no-textconv", "-L --date").allows(args)
	case "shortlog":
		return flags(true, "-n -s -e --numbered --summary --email --all", "--group --format").allows(args)
	case "grep":
		return flags(true, "-i -v -w -F -E -G -P -n -H -h -l -L -c -z --cached --no-index --untracked --recurse-submodules --no-textconv --quiet --only-matching", "-e -f -A -B -C --max-depth").allows(args)
	}
	return false
}

func trustedReadOnlyCommand(name string) bool {
	dir, err := os.Getwd()
	if err != nil {
		return false
	}
	path, absolute := executable.Resolve(name, os.Getenv("PATH"), dir)
	return absolute && executable.Trusted(path)
}
