// Command ccdp is a TUI coding agent written in Go, combining the strengths
// of OpenAI Codex (event-driven agent loop, approval gates, session
// persistence) and Claude Code (permission modes, context compaction,
// interactive chat).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/tui"
)

const version = "0.2.0"

// Keep command-line prompt sources bounded before they reach the runtime
// prompt/history budget. The limit is intentionally generous for source
// context, but prevents an accidental pipe or file from consuming memory.
const maxCLIPromptBytes = 4 << 20

func main() {
	var (
		effortFlag    = flag.String("effort", "", "reasoning effort (empty = built-in default medium)")
		verbosityFlag = flag.String("verbosity", "", "response verbosity: low|medium|high (empty = provider default)")
		modelFlag     = flag.String("m", "", "model to use (overrides config)")
		dirFlag       = flag.String("d", "", "working directory (default: current directory)")
		resumeFlag    = flag.String("r", "", "resume session by id")
		continueFlag  = flag.Bool("c", false, "open the session picker on start (continue a session)")
		replayFlag    = flag.String("replay", "", "print a saved session as a markdown replay and exit")
		modeFlag      = flag.String("mode", "", "permission mode: manual|edits|bypass (default: edits)")
		listSessions  = flag.Bool("l", false, "list saved sessions and exit")
		showVersion   = flag.Bool("version", false, "print version and exit")
		noWarnMissing = flag.Bool("y", false, "do not prompt on missing config")
		promptFlag    = flag.String("p", "", "run one prompt in headless mode, print the response and exit")
		outputFlag    = flag.String("output-format", "text", "headless output format: text | json | stream-json")
		inputFormat   = flag.String("input-format", "text", "headless input format: text | stream-json")
		timeoutFlag   = flag.Int("timeout", 600, "headless mode timeout in seconds")
		maxTurnsFlag  = flag.Int("max-turns", 0, "cap the model-call iterations per turn (0 = unlimited)")
		maxBudgetFlag = flag.Float64("max-budget", 0, "hard USD spending cap for the session (0 = unlimited)")
		configFlag    = flag.String("config-file", "", "path to a config file (default ~/.ccdp/config.json)")
		appendPrompt  = flag.String("append-system-prompt", "", "extra instructions appended to the system prompt")
		allowTools    = flag.String("allowedTools", "", "comma-separated tools always allowed (overrides the gate)")
		denyTools     = flag.String("disallowedTools", "", "comma-separated tools always denied")
		verboseFlag   = flag.Bool("verbose", false, "print extra progress diagnostics to stderr")
		debugFlag     = flag.Bool("debug", false, "print LLM request details to stderr")
		sessionIDFlag = flag.String("session-id", "", "use a specific session id instead of generating one")
		sysPromptFlag = flag.String("system-prompt", "", "override the system prompt")
		sysPromptFile = flag.String("system-prompt-file", "", "read the system prompt from a file")
		addDirFlag    = flag.String("add-dir", "", "grant the sandbox access to an additional directory")
		noPersistFlag = flag.Bool("no-session-persistence", false, "do not write session files to disk")
		fallbackFlag  = flag.String("fallback-model", "", "model to fall back to when the primary fails")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `ccdp %s — a TUI coding agent (Codex-style loop, Claude-style UX)

Usage:
  ccdp [flags]

Flags:
`, version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("ccdp " + version)
		return
	}

	cfg, err := loadConfig(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	cliValues := map[string]any{
		"m":                      *modelFlag,
		"d":                      *dirFlag,
		"r":                      *resumeFlag,
		"mode":                   *modeFlag,
		"max-turns":              *maxTurnsFlag,
		"max-budget":             *maxBudgetFlag,
		"append-system-prompt":   *appendPrompt,
		"allowedTools":           *allowTools,
		"disallowedTools":        *denyTools,
		"verbose":                *verboseFlag,
		"debug":                  *debugFlag,
		"session-id":             *sessionIDFlag,
		"system-prompt":          *sysPromptFlag,
		"system-prompt-file":     *sysPromptFile,
		"add-dir":                *addDirFlag,
		"no-session-persistence": *noPersistFlag,
		"fallback-model":         *fallbackFlag,
		"effort":                 *effortFlag,
		"verbosity":              *verbosityFlag,
	}
	// A file flag is read only when present and non-empty.  An explicitly empty
	// path still participates in provenance, but retains the already resolved
	// prompt rather than accidentally reading the process working directory.
	if cliFlagWasVisited(flag.CommandLine, "system-prompt-file") && *sysPromptFile != "" {
		data, readErr := readBoundedFile(*sysPromptFile, maxCLIPromptBytes)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "error: system-prompt-file:", readErr)
			os.Exit(1)
		}
		cliValues["system-prompt-file"] = string(data)
	} else if cliFlagWasVisited(flag.CommandLine, "system-prompt-file") {
		cliValues["system-prompt-file"] = nil
	}
	if _, err := applyCLIOverrides(&cfg, flag.CommandLine, cliValues); err != nil {
		fmt.Fprintln(os.Stderr, "error: CLI override:", err)
		os.Exit(1)
	}
	if _, err := permissions.ParseMode(cfg.PermissionMode); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Resolve workspace.
	workspaceInput := cfg.Workspace
	dirProvided := cliFlagWasVisited(flag.CommandLine, "d")
	if dirProvided {
		workspaceInput = *dirFlag
	}
	if strings.TrimSpace(workspaceInput) == "" {
		workspaceInput = "."
	}
	ws, err := filepath.Abs(workspaceInput)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: workspace:", err)
		os.Exit(1)
	}
	info, err := os.Stat(ws)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: workspace %q is not a directory\n", ws)
		os.Exit(1)
	}
	if dirProvided {
		if err := cfg.ApplyCLIOverride("workspace", ws); err != nil {
			fmt.Fprintln(os.Stderr, "error: workspace CLI override:", err)
			os.Exit(1)
		}
	} else {
		cfg.Workspace = ws
	}

	if *listSessions {
		sessions, issues, err := agent.ListSessions(cfg.SessionDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		for _, issue := range issues {
			fmt.Fprintf(os.Stderr, "warning: skipping session %s: %s\n", issue.ID, issue.Reason)
		}
		if len(sessions) == 0 {
			fmt.Println("no saved sessions")
			return
		}
		for _, s := range sessions {
			fmt.Printf("%s  %s  %s  %d messages\n", s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Model, len(s.History))
		}
		return
	}

	if *replayFlag != "" {
		snap, err := agent.LoadSession(cfg.SessionDir, *replayFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: cannot load session:", err)
			os.Exit(1)
		}
		// Replay is strictly read-only: project the JSONL/legacy snapshot and
		// render it without constructing an Agent, MCP host, or writer lock.
		fmt.Println(agent.ExportSessionMarkdown(snap))
		return
	}

	if cfg.ResolveProvider(cfg.Model).APIKey == "" && !*noWarnMissing {
		fmt.Fprintln(os.Stderr, "warning: no API key configured for the selected provider (api_key or api_key_env)")
	}

	var ag *agent.Agent
	if *resumeFlag != "" {
		snap, err := agent.LoadSession(cfg.SessionDir, *resumeFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot resume session %q: %v\n", *resumeFlag, err)
			os.Exit(1)
		}
		ag, err = agent.Resume(&cfg, snap, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "resumed session %s\n", *resumeFlag)
	} else {
		ag, err = agent.New(&cfg, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	// Resolve headless prompts. -p wins; --input-format stream-json reads
	// multi-turn JSONL from stdin; otherwise piped stdin is one text prompt.
	var prompts []string
	switch *inputFormat {
	case "stream-json":
		if *promptFlag != "" {
			fmt.Fprintln(os.Stderr, "error: -p and --input-format stream-json are mutually exclusive")
			os.Exit(1)
		}
		if *resumeFlag != "" {
			fmt.Fprintln(os.Stderr, "error: -r and --input-format stream-json are mutually exclusive")
			os.Exit(1)
		}
		ps, err := parseStreamJSONInput(bufio.NewReader(os.Stdin))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		prompts = ps
	default:
		headlessPrompt := *promptFlag
		if headlessPrompt == "" && *resumeFlag == "" {
			if st, _ := os.Stdin.Stat(); st != nil && st.Mode()&os.ModeCharDevice == 0 {
				data, readErr := io.ReadAll(io.LimitReader(os.Stdin, maxCLIPromptBytes+1))
				if readErr != nil {
					fmt.Fprintln(os.Stderr, "error: stdin:", readErr)
					os.Exit(1)
				}
				if len(data) > maxCLIPromptBytes {
					fmt.Fprintf(os.Stderr, "error: stdin prompt exceeds %d bytes\n", maxCLIPromptBytes)
					os.Exit(1)
				}
				if s := strings.TrimSpace(string(data)); s != "" {
					headlessPrompt = s
				}
			}
		}
		if headlessPrompt != "" {
			prompts = []string{headlessPrompt}
		}
	}

	// The TUI shows a welcome banner itself for fresh sessions.
	// Configure the human channel before recovery can launch a background run.
	ag.SetChildInteraction(len(prompts) == 0)
	go ag.Run()

	if len(prompts) > 0 {
		code := runHeadless(ag, prompts, outputFormat(*outputFlag), *timeoutFlag)
		if !cfg.NoSessionPersistence {
			if saveErr := ag.Save(); saveErr != nil {
				fmt.Fprintln(os.Stderr, "error: save session:", saveErr)
				if code == 0 {
					code = 1
				}
			}
		}
		if closeErr := ag.CloseContext(context.Background()); closeErr != nil {
			fmt.Fprintln(os.Stderr, "error: close session:", closeErr)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	}

	p := tui.NewProgram(
		tui.NewWithResume(ag, *continueFlag),
	)

	finalModel, runErr := p.Run()
	// A resume swaps the Agent handle inside the TUI. Use the final model's
	// current handle for shutdown; the startup handle may now be retired.
	finalAgent := ag
	current, tuiCloseErr := shutdownTUIModel(finalModel)
	if current != nil {
		finalAgent = current
	}
	if runErr != nil {
		if tuiCloseErr != nil {
			fmt.Fprintln(os.Stderr, "error: close TUI subscription:", tuiCloseErr)
		}
		if finalAgent != nil {
			if !cfg.NoSessionPersistence {
				if saveErr := finalAgent.Save(); saveErr != nil {
					fmt.Fprintln(os.Stderr, "error: save session:", saveErr)
				}
			}
			if closeErr := finalAgent.CloseContext(context.Background()); closeErr != nil {
				fmt.Fprintln(os.Stderr, "error: close session:", closeErr)
			}
		}
		fmt.Fprintln(os.Stderr, "error:", runErr)
		os.Exit(1)
	}
	if tuiCloseErr != nil {
		fmt.Fprintln(os.Stderr, "error: close TUI subscription:", tuiCloseErr)
	}
	if finalAgent == nil {
		fmt.Fprintln(os.Stderr, "error: TUI returned no session handle")
		os.Exit(1)
	}
	finalID := finalAgent.SessionID()
	var saveErr error
	if !cfg.NoSessionPersistence {
		saveErr = finalAgent.Save()
	}
	closeErr := finalAgent.CloseContext(context.Background())
	if saveErr != nil {
		fmt.Fprintln(os.Stderr, "error: save session:", saveErr)
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "error: close session:", closeErr)
	}
	if tuiCloseErr != nil || saveErr != nil || closeErr != nil {
		os.Exit(1)
	}
	if cfg.NoSessionPersistence {
		fmt.Println("\nbye — session " + finalID + " closed (session persistence disabled)")
	} else {
		fmt.Println("\nbye — session " + finalID + " saved")
	}
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	// A path may be swapped after Stat. Non-blocking open plus a second stat
	// prevents a FIFO from wedging startup while the bounded reader is waiting
	// for a writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("file is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func loadConfig(path string) (config.Config, error) {
	if path == "" {
		return config.Load()
	}
	return config.LoadFrom(path)
}

// cliFlagWasVisited reports presence independently of a flag's value. The
// standard flag package otherwise makes --max-turns=0, --debug=false and an
// explicitly empty string indistinguishable from an omitted flag.
func cliFlagWasVisited(fs *flag.FlagSet, name string) bool {
	if fs == nil {
		return false
	}
	visited := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			visited = true
		}
	})
	return visited
}

// applyCLIOverrides is the single CLI-to-config boundary. Values are passed
// through Config.ApplyCLIOverride so every explicit flag (including false,
// zero and empty values) carries SourceCLI provenance and project settings
// cannot later override it. The returned map is also used for path flags whose
// final value is normalized after this pass.
func applyCLIOverrides(cfg *config.Config, fs *flag.FlagSet, values map[string]any) (map[string]bool, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	visited := make(map[string]bool)
	if fs != nil {
		fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	}
	valueString := func(name string) (string, error) {
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("missing value for -%s", name)
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("value for -%s is %T, want string", name, value)
		}
		return text, nil
	}
	apply := func(flagName, field string, value any) error {
		if !visited[flagName] {
			return nil
		}
		if err := cfg.ApplyCLIOverride(field, value); err != nil {
			return fmt.Errorf("-%s: %w", flagName, err)
		}
		return nil
	}
	parseList := func(name string) ([]string, error) {
		raw, err := valueString(name)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(raw) == "" {
			return []string{}, nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	}

	// Keep precedence deterministic for flags that intentionally compose or
	// replace the same setting. This mirrors the documented behavior: an
	// explicit system-prompt-file wins over --system-prompt, which wins over
	// --append-system-prompt.
	ordered := []struct {
		flagName string
		field    string
	}{
		{"m", "model"},
		{"mode", "permission_mode"},
		{"max-turns", "max_turns"},
		{"max-budget", "max_budget_usd"},
		{"d", "workspace"},
		{"session-id", "session_id"},
		{"fallback-model", "fallback_model"},
		{"effort", "reasoning_effort"},
		{"verbosity", "verbosity"},
		{"no-session-persistence", "no_session_persistence"},
	}
	for _, item := range ordered {
		if !visited[item.flagName] {
			continue
		}
		if err := apply(item.flagName, item.field, values[item.flagName]); err != nil {
			return visited, err
		}
	}
	if visited["allowedTools"] {
		list, err := parseList("allowedTools")
		if err != nil {
			return visited, fmt.Errorf("-allowedTools: %w", err)
		}
		if err := apply("allowedTools", "always_allow", list); err != nil {
			return visited, err
		}
	}
	if visited["disallowedTools"] {
		list, err := parseList("disallowedTools")
		if err != nil {
			return visited, fmt.Errorf("-disallowedTools: %w", err)
		}
		if err := apply("disallowedTools", "always_deny", list); err != nil {
			return visited, err
		}
	}
	if visited["add-dir"] {
		dir, err := valueString("add-dir")
		if err != nil {
			return visited, err
		}
		var dirs []string
		if strings.TrimSpace(dir) != "" {
			dirs = []string{strings.TrimSpace(dir)}
		} else {
			dirs = []string{}
		}
		if err := apply("add-dir", "additional_directories", dirs); err != nil {
			return visited, err
		}
	}
	if visited["verbose"] {
		verbose, ok := values["verbose"].(bool)
		if !ok {
			return visited, fmt.Errorf("-verbose: value is %T, want bool", values["verbose"])
		}
		if debug, ok := values["debug"].(bool); visited["debug"] && ok && debug {
			verbose = true
		}
		if err := apply("verbose", "verbose", verbose); err != nil {
			return visited, err
		}
	}
	if visited["debug"] {
		debug, ok := values["debug"].(bool)
		if !ok {
			return visited, fmt.Errorf("-debug: value is %T, want bool", values["debug"])
		}
		if err := apply("debug", "debug", debug); err != nil {
			return visited, err
		}
		if debug && !visited["verbose"] {
			if err := cfg.ApplyCLIOverride("verbose", true); err != nil {
				return visited, err
			}
		}
	}
	if visited["append-system-prompt"] {
		text, err := valueString("append-system-prompt")
		if err != nil {
			return visited, err
		}
		if err := apply("append-system-prompt", "system_prompt", cfg.SystemPrompt+"\n\n"+text); err != nil {
			return visited, err
		}
	}
	if visited["system-prompt"] {
		if err := apply("system-prompt", "system_prompt", values["system-prompt"]); err != nil {
			return visited, err
		}
	}
	if visited["system-prompt-file"] {
		value := values["system-prompt-file"]
		if value == nil {
			value = cfg.SystemPrompt
		}
		if err := apply("system-prompt-file", "system_prompt", value); err != nil {
			return visited, err
		}
	}
	return visited, nil
}

// shutdownTUIModel handles both concrete forms Bubble Tea may return. Most
// command/key paths return *tui.Model while constructors and some message
// paths return tui.Model; missing the pointer form would save/close the
// startup Agent after a successful resume.
func shutdownTUIModel(model tea.Model) (*agent.Agent, error) {
	switch final := model.(type) {
	case tui.Model:
		return final.CurrentAgent(), final.Close()
	case *tui.Model:
		if final == nil {
			return nil, nil
		}
		return final.CurrentAgent(), final.Close()
	default:
		return nil, nil
	}
}
