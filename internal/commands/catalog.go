// Package commands contains the command metadata shared by interactive and
// headless clients.  It intentionally has no dependency on Bubble Tea or on
// the runtime: command metadata is data, while execution belongs to a client
// and ultimately goes through protocol.SessionClient.
package commands

import (
	"sort"
	"strings"
)

// Feedback describes how a command's result is presented by a client.
type Feedback string

const (
	FeedbackReport    Feedback = "report"
	FeedbackSelector  Feedback = "selector"
	FeedbackNotice    Feedback = "notice"
	FeedbackOperation Feedback = "operation"
)

// BusyRule describes admission while a session has an active operation.  The
// runtime remains authoritative; this value is only used by clients to give
// an early, consistent explanation and to select an appropriate presentation.
type BusyRule string

const (
	BusyAllow  BusyRule = "allow"
	BusyReject BusyRule = "reject"
	BusyQueue  BusyRule = "queue"
)

// Entry is the complete metadata for one slash command. Name is the stable
// canonical command name; aliases are alternate spellings accepted by the
// parser and completion UI.
type Entry struct {
	Name                 string
	Aliases              []string
	Help                 string
	Feedback             Feedback
	Busy                 BusyRule
	Transcript           bool
	TransientSubcommands []string
	Mutation             bool
}

// Catalog is an immutable command directory after construction. Methods
// return copies, so clients cannot mutate the shared command metadata.
type Catalog struct {
	entries []Entry
	byName  map[string]Entry
	byID    map[string]string
}

// New builds a catalog from entries. Names are normalized to lower case and
// duplicate aliases are rejected by retaining the first declaration. The
// returned catalog owns copies of all caller-provided slices.
func New(entries []Entry) Catalog {
	c := Catalog{byName: make(map[string]Entry), byID: make(map[string]string)}
	for _, src := range entries {
		name := strings.ToLower(strings.TrimSpace(src.Name))
		if name == "" {
			continue
		}
		e := src
		e.Name = name
		e.Aliases = normalizeNames(src.Aliases)
		e.TransientSubcommands = normalizeNames(src.TransientSubcommands)
		if e.Feedback == "" {
			e.Feedback = FeedbackReport
		}
		if e.Busy == "" {
			e.Busy = BusyAllow
		}
		if _, exists := c.byName[name]; exists {
			continue
		}
		c.entries = append(c.entries, e)
		c.byName[name] = e
		c.byID[name] = name
		for _, alias := range e.Aliases {
			if _, exists := c.byName[alias]; exists {
				continue
			}
			c.byName[alias] = e
			c.byID[alias] = name
		}
	}
	return c
}

func normalizeNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		name = strings.Trim(name, "/")
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Entries returns canonical entries in stable alphabetical order. Aliases
// remain attached to their canonical entry.
func (c Catalog) Entries() []Entry {
	out := make([]Entry, len(c.entries))
	for i, e := range c.entries {
		out[i] = e
		out[i].Aliases = append([]string(nil), e.Aliases...)
		out[i].TransientSubcommands = append([]string(nil), e.TransientSubcommands...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ShouldTranscript centralizes the feedback lifetime for a command and its
// first subcommand. Clients should not maintain a second switch that guesses
// whether a command's output is durable or transient.
func (c Catalog) ShouldTranscript(name string, args []string) bool {
	e, ok := c.Lookup(name)
	if !ok {
		return true
	}
	if !e.Transcript {
		return false
	}
	if len(args) > 0 {
		for _, transient := range e.TransientSubcommands {
			if strings.EqualFold(args[0], transient) {
				return false
			}
		}
	}
	return true
}

// Lookup resolves a canonical name or alias. A leading slash is accepted for
// callers that pass the raw token from a composer.
func (c Catalog) Lookup(name string) (Entry, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "/")
	e, ok := c.byName[name]
	if !ok {
		return Entry{}, false
	}
	e.Aliases = append([]string(nil), e.Aliases...)
	e.TransientSubcommands = append([]string(nil), e.TransientSubcommands...)
	return e, true
}

// Canonical returns the stable name for a canonical name or alias.
func (c Catalog) Canonical(name string) (string, bool) {
	name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))
	canonical, ok := c.byID[name]
	return canonical, ok
}

// Names returns every accepted spelling (canonical names and aliases) in
// stable order. It is the source for completion; custom commands are added by
// the caller because their scope and trust are session/workspace specific.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c.byName))
	for name := range c.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Suggestions returns all accepted spellings with the supplied prefix. The
// prefix may include a leading slash but must not include arguments.
func (c Catalog) Suggestions(prefix string) []string {
	prefix = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(prefix), "/"))
	var out []string
	for _, name := range c.Names() {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	return out
}

// Help renders the command directory. This text is generated from the same
// entries used for completion, so help and suggestions cannot drift.
func (c Catalog) Help() string {
	entries := c.Entries()
	maxName := len("command")
	for _, e := range entries {
		label := "/" + e.Name
		if len(e.Aliases) > 0 {
			label += " (" + strings.Join(prefixed(e.Aliases), ", ") + ")"
		}
		if len(label) > maxName {
			maxName = len(label)
		}
	}
	var b strings.Builder
	b.WriteString("ccdp commands\n")
	b.WriteString(strings.Repeat("─", maxName+35))
	b.WriteByte('\n')
	for _, e := range entries {
		label := "/" + e.Name
		if len(e.Aliases) > 0 {
			label += " (" + strings.Join(prefixed(e.Aliases), ", ") + ")"
		}
		b.WriteString(label)
		b.WriteString(strings.Repeat(" ", maxName-len(label)+2))
		b.WriteString(e.Help)
		b.WriteByte('\n')
	}
	b.WriteString("\nCustom commands: markdown files in ~/.ccdp/commands/<name>.md or .ccdp/commands/<name>.md become /name; $ARGUMENTS is replaced by the args.\n")
	b.WriteString("Keys\n  Enter        send/steer; /next queues      ⌥Enter       newline (⌃J also works)\n  ↑/↓          edit multiline input or browse input history\n  Home/End     move within input\n  ⌃C           interrupt agent; press twice when idle to quit\n  ⌘C           native terminal copy\n  trackpad     native history; PgUp/PgDn or ⌃U/⌃D scroll the view\n  ⌃O           read transcript (review details in approval)\n  ⌃A           browse agents\n  Approval     ↑/↓ choose · Enter confirm · Esc later · /pending reopen\n  esc          dismiss or return; interrupt when running\n  /transcript  read full messages and tool output\n  Reader       / search · n next · o raw · c copy · esc return\n")
	return b.String()
}

func prefixed(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = "/" + name
	}
	return out
}

// Default is the command directory for the current CLI. It intentionally
// includes init, which historically existed in the dispatcher but was absent
// from the old help parser, and ? as the documented help alias.
func Default() Catalog {
	return New([]Entry{
		{Name: "next", Help: "<message> queue input for the next turn", Feedback: FeedbackOperation, Busy: BusyAllow, Mutation: true},
		{Name: "reconnect", Help: "reattach to session updates without resending commands", Feedback: FeedbackNotice, Busy: BusyAllow},
		{Name: "pending", Help: "reopen a deferred question or approval", Feedback: FeedbackSelector, Busy: BusyAllow},
		{Name: "tasks", Help: "toggle the current task checklist", Feedback: FeedbackSelector, Busy: BusyAllow},
		{Name: "agents", Help: "browse child agents, progress and pending approvals", Feedback: FeedbackSelector, Busy: BusyAllow},
		{Name: "agent", Help: "<id> [send|followup|interrupt|cancel|continue <text>]", Busy: BusyAllow},
		{Name: "parent", Help: "view the parent agent without stopping execution", Busy: BusyAllow},
		{Name: "root", Help: "view the main agent without stopping execution", Busy: BusyAllow},
		{Name: "transcript", Aliases: []string{"details"}, Help: "read full messages and tool output; search, copy and return", Feedback: FeedbackSelector, Busy: BusyAllow},
		{Name: "agent-history", Help: "[before] browse saved messages and tool output", Busy: BusyAllow},
		{Name: "agent-output", Help: "<item-id> [offset] read a full saved output page", Busy: BusyAllow},
		{Name: "stop-tree", Help: "cancel and join the main run and all child runs", Busy: BusyAllow, Mutation: true},
		{Name: "help", Aliases: []string{"?"}, Help: "show this help", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "clear", Help: "clear conversation history", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "compact", Help: "compact context now", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "cost", Help: "show token usage and estimated cost", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "context", Help: "visualize context window use and cumulative tokens", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "config", Help: "show runtime configuration or set a session setting", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true, TransientSubcommands: []string{"set"}},
		{Name: "doctor", Help: "run environment diagnostics", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "permissions", Help: "show or manage permission rules", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true, TransientSubcommands: []string{"allow", "deny", "remove"}, Mutation: true},
		{Name: "memory", Help: "show or clear session memory", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true, TransientSubcommands: []string{"clear"}, Mutation: true},
		{Name: "github", Help: "show GitHub integration status", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "pr-comments", Help: "fetch review comments on the current PR", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "commit-push-pr", Help: "commit, push and open a PR for the current branch", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "reload", Help: "reload .ccdp/settings.json at runtime", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "plugins", Help: "show loaded plugins and LLM providers", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "mcp", Help: "show connected MCP servers, tools, resources, prompts", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "skills", Help: "list available skills", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "mode", Help: "choose or switch permission mode", Feedback: FeedbackSelector, Busy: BusyAllow, Mutation: true},
		{Name: "model", Help: "choose or switch the active model", Feedback: FeedbackSelector, Busy: BusyAllow, Mutation: true},
		{Name: "effort", Help: "choose reasoning effort with left/right; default resets to built-in medium", Feedback: FeedbackSelector, Busy: BusyAllow, Mutation: true},
		{Name: "verbosity", Help: "choose response verbosity: low|medium|high|default", Feedback: FeedbackSelector, Busy: BusyAllow, Mutation: true},
		{Name: "plan", Help: "toggle plan mode (propose-then-approve)", Feedback: FeedbackSelector, Busy: BusyAllow, Mutation: true},
		{Name: "sandbox", Help: "choose or switch sandbox mode", Feedback: FeedbackSelector, Busy: BusyAllow, Mutation: true},
		{Name: "remove", Help: "remove the last n messages", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "rewind", Help: "keep the first n messages or pick interactively", Feedback: FeedbackSelector, Busy: BusyReject, Mutation: true},
		{Name: "fork", Help: "branch a new session from message n", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "checkpoint", Help: "create or restore a checkpoint", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "review", Help: "show changes and checkpoints", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "add-dir", Help: "allow the sandbox to touch another directory", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "disallowed-dir", Help: "block a directory in the sandbox", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "export", Help: "export the conversation as markdown", Feedback: FeedbackOperation, Busy: BusyAllow, Mutation: true},
		{Name: "diff", Help: "show git status and a diff of changes", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "cd", Help: "change the working directory", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "trust", Help: "trust or untrust the current project", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "pwd", Help: "show the current working directory", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "statusline", Help: "show or set statusline items", Feedback: FeedbackSelector, Busy: BusyAllow},
		{Name: "apply", Help: "apply a git patch or send a plan to the agent", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
		{Name: "git", Help: "run a git command in the workspace", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true, Mutation: true},
		{Name: "save", Help: "save the session to disk", Feedback: FeedbackOperation, Busy: BusyAllow, Mutation: true},
		{Name: "resume", Help: "resume a saved session", Feedback: FeedbackSelector, Busy: BusyReject, Mutation: true},
		{Name: "status", Help: "show session details", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "commands", Help: "list custom slash commands", Feedback: FeedbackReport, Busy: BusyAllow, Transcript: true},
		{Name: "quit", Aliases: []string{"exit"}, Help: "quit ccdp", Feedback: FeedbackNotice, Busy: BusyAllow},
		{Name: "init", Help: "create project instruction file", Feedback: FeedbackOperation, Busy: BusyReject, Mutation: true},
	})
}
