package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/skills"
)

// queryRuntimeSnapshot is the read-only boundary for typed queries.  In
// particular, cfg, permission state, sandbox state, catalog and revision are
// captured while holding Agent.mu; report rendering must not call one of the
// older convenience methods that re-reads live state after that boundary.
type queryRuntimeSnapshot struct {
	view          protocol.SessionView
	cfg           config.Config
	sandboxReady  bool
	sandboxDetail string
	sandboxRev    uint64
	sandboxLimits bool
	plugins       []string
	pending       []string
	mcpNames      []string
	mcpTools      map[string][]string
	skills        []skills.Skill
	permissions   permissions.Snapshot
}

// Query returns one of the bounded, credential-free reports exposed by the
// typed command protocol. It is deliberately read-only: report generation
// must not mutate session history or create a durable fact.
func (a *Agent) Query(ctx context.Context, kind protocol.QueryKind) (protocol.QueryReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return protocol.QueryReport{}, ctx.Err()
	default:
	}

	snapshot := a.captureQuerySnapshot(ctx)
	if snapshot == nil {
		return protocol.QueryReport{}, ctx.Err()
	}
	var text string
	var err error
	switch kind {
	case protocol.QueryConfig:
		text = formatQueryConfig(snapshot)
	case protocol.QueryDoctor:
		text = formatQueryDoctor(snapshot)
	case protocol.QueryPermissions:
		text = formatQueryPermissions(snapshot)
	case protocol.QueryMemory:
		text = a.MemoryText()
	case protocol.QueryMCP:
		text = formatQueryMap(snapshot.mcpTools)
	case protocol.QueryPlugins:
		text = formatQueryPlugins(snapshot)
	case protocol.QuerySkills:
		text = formatQuerySkills(snapshot.skills)
	case protocol.QueryGitHub:
		text = a.GitHubStatusContext(ctx)
	case protocol.QueryStatus:
		var data []byte
		data, err = json.Marshal(snapshot.view)
		text = string(data)
	default:
		return protocol.QueryReport{}, fmt.Errorf("unknown query kind %q", kind)
	}
	if err != nil {
		return protocol.QueryReport{}, err
	}
	select {
	case <-ctx.Done():
		return protocol.QueryReport{}, ctx.Err()
	default:
	}
	return protocol.QueryReport{Kind: kind, Revision: snapshot.view.Revision, Text: boundedCommandOutput(text)}, nil
}

func (a *Agent) captureQuerySnapshot(ctx context.Context) *queryRuntimeSnapshot {
	if a == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	// The settings publication lock spans cfg, policy, hooks, MCP and registry
	// swaps. Taking its read side before Agent.mu gives typed queries one
	// coherent generation even while a worker prepares a replacement.
	a.settingsCommitMu.RLock()
	defer a.settingsCommitMu.RUnlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	view := a.snapshotLocked()
	cfg := cloneConfig(a.cfg)
	var sandboxReady bool
	var sandboxDetail string
	var sandboxRevision uint64
	var limits bool
	if a.sandbox != nil {
		sandboxRevision = a.sandbox.Revision
		limits = a.sandbox.Limits != nil
		if err := a.sandbox.BackendError(); err != nil {
			sandboxDetail = err.Error()
		} else {
			sandboxReady = true
			sandboxDetail = "macOS Seatbelt is available"
		}
	} else {
		sandboxDetail = "sandbox policy is unavailable"
	}
	var plugins, pending []string
	if a.host != nil {
		plugins = a.host.Names()
		pending = a.host.Pending()
	}
	var mcpNames []string
	mcpTools := map[string][]string{}
	if a.mcp != nil {
		mcpNames = a.mcp.Names()
		for name, names := range a.mcp.ToolNames() {
			mcpTools[name] = append([]string(nil), names...)
		}
	}
	var loadedSkills []skills.Skill
	if a.skills != nil {
		loadedSkills = a.skills.All()
	}
	var permissionSnapshot permissions.Snapshot
	if a.perms != nil {
		permissionSnapshot = a.perms.Snapshot()
	}
	return &queryRuntimeSnapshot{view: view, cfg: cfg, sandboxReady: sandboxReady,
		sandboxDetail: sandboxDetail, sandboxRev: sandboxRevision,
		sandboxLimits: limits, plugins: append([]string(nil), plugins...),
		pending: append([]string(nil), pending...), mcpNames: append([]string(nil), mcpNames...),
		mcpTools: mcpTools, skills: loadedSkills, permissions: permissionSnapshot}
}

func formatQueryConfig(s *queryRuntimeSnapshot) string {
	if s == nil {
		return ""
	}
	cfg := s.cfg
	mode := s.permissions.Mode
	if mode == "" {
		mode = permissions.Mode(cfg.PermissionMode)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model:         %s\n", cfg.Model)
	fmt.Fprintf(&b, "reasoning_effort: %s\nverbosity: %s\n", cfg.ReasoningEffortFor(cfg.Model), cfg.Verbosity)
	fmt.Fprintf(&b, "base_url:      %s\n", redactURL(cfg.ResolveProvider(cfg.Model).BaseURL))
	fmt.Fprintf(&b, "api_key:       %s\n", maskKey(cfg.ResolveProvider(cfg.Model).APIKey))
	fmt.Fprintf(&b, "permission:    %s\n", mode)
	fmt.Fprintf(&b, "Seatbelt:      %s", s.sandboxDetail)
	if s.sandboxLimits {
		b.WriteString(" (limits: on)")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "sandbox revision: %d\n", s.sandboxRev)
	fmt.Fprintf(&b, "network access: %v\n", cfg.NetworkAccess)
	fmt.Fprintf(&b, "workspace:     %s\n", cfg.Workspace)
	fmt.Fprintf(&b, "read/write roots: %s\n", strings.Join(append([]string{cfg.Workspace}, cfg.AdditionalDirectories...), ", "))
	fmt.Fprintf(&b, "read-only roots: %s\n", strings.Join(cfg.AdditionalReadOnlyDirectories, ", "))
	fmt.Fprintf(&b, "denied roots: %s\n", strings.Join(cfg.DisallowedDirectories, ", "))
	fmt.Fprintf(&b, "plan mode:     %v\n", s.view.Settings.ExecutionMode == protocol.ExecutionModePlan)
	fmt.Fprintf(&b, "workspace:     %s\n", cfg.Workspace)
	fmt.Fprintf(&b, "context_window:%d\n", cfg.ContextWindowFor(cfg.Model))
	fmt.Fprintf(&b, "compact_thr:   %.0f%%\n", cfg.CompactThreshold*100)
	fmt.Fprintf(&b, "max_turns:     %d\n", cfg.MaxTurns)
	if cfg.MaxBudgetUSD > 0 {
		fmt.Fprintf(&b, "max_budget:    $%.2f\n", cfg.MaxBudgetUSD)
	}
	fmt.Fprintf(&b, "tools:         %d\n", len(s.view.Catalog.Tools))
	fmt.Fprintf(&b, "plugins:       %d\n", len(s.plugins))
	fmt.Fprintf(&b, "mcp servers:   %d\n", len(s.mcpNames))
	fmt.Fprintf(&b, "skills:        %d\n", len(s.skills))
	path := cfg.SourcePath()
	if path == "" {
		path = config.ConfigPath()
	}
	fmt.Fprintf(&b, "config file:   %s\n", path)
	if report := cfg.SourceReport(); len(report.Fields) > 0 {
		b.WriteString("sources:\n")
		fields := make([]string, 0, len(report.Fields))
		for field := range report.Fields {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			p := report.Fields[field]
			if !p.Explicit {
				continue
			}
			fmt.Fprintf(&b, "  %s: %s\n", field, p.Source)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatQueryDoctor(s *queryRuntimeSnapshot) string {
	if s == nil {
		return ""
	}
	cfg := s.cfg
	var b strings.Builder
	ok := func(name string, pass bool, detail string) {
		status := "✓"
		if !pass {
			status = "✗"
		}
		if detail != "" {
			fmt.Fprintf(&b, "%s %s — %s\n", status, name, detail)
		} else {
			fmt.Fprintf(&b, "%s %s\n", status, name)
		}
	}
	st, statErr := os.Stat(cfg.Workspace)
	ok("workspace exists", statErr == nil && st.IsDir(), cfg.Workspace)
	workspaceWritable, _ := probeWritableDir(cfg.Workspace, ".ccdp-doctor-*")
	ok("workspace writable", workspaceWritable, "")
	sessionWritable, sessionDetail := probeWritableDir(cfg.SessionDir, ".ccdp-doctor-*")
	ok("session dir writable", sessionWritable, sessionDetail)
	ok("api key configured", cfg.APIKey != "", "")
	ok("base_url set", cfg.BaseURL != "", redactURL(cfg.ResolveProvider(cfg.Model).BaseURL))
	path := cfg.SourcePath()
	if path == "" {
		path = config.ConfigPath()
	}
	ok("config file present", fileExists(path), path)
	ok("Seatbelt backend", s.sandboxReady, s.sandboxDetail)
	ok("tools loaded", len(s.view.Catalog.Tools) > 0, fmt.Sprintf("%d tools", len(s.view.Catalog.Tools)))
	if len(s.mcpNames) > 0 {
		ok("mcp servers", true, fmt.Sprintf("%d connected", len(s.mcpNames)))
	} else {
		ok("mcp servers", true, "none configured")
	}
	ok("skills loaded", true, fmt.Sprintf("%d skills", len(s.skills)))
	return strings.TrimRight(b.String(), "\n")
}

func formatQueryPermissions(s *queryRuntimeSnapshot) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Permission mode: %s\n", s.permissions.Mode)
	b.WriteString("Always allow:\n")
	if len(s.permissions.Policy.AlwaysAllow) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, rule := range s.permissions.Policy.AlwaysAllow {
		b.WriteString("  - " + rule + "\n")
	}
	b.WriteString("Always deny:\n")
	if len(s.permissions.Policy.AlwaysDeny) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, rule := range s.permissions.Policy.AlwaysDeny {
		b.WriteString("  - " + rule + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatQueryPlugins(s *queryRuntimeSnapshot) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	for _, name := range s.plugins {
		b.WriteString(name + "\n")
	}
	for _, name := range s.pending {
		fmt.Fprintf(&b, "%s (pending)\n", name)
	}
	if len(s.plugins) == 0 && len(s.pending) == 0 {
		b.WriteString("(none)\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatQuerySkills(all []skills.Skill) string {
	if len(all) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, skill := range all {
		description := strings.ReplaceAll(skill.Description, "\n", " ")
		if description == "" {
			b.WriteString(skill.Name + "\n")
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", skill.Name, description)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatQueryMap(values map[string][]string) string {
	if len(values) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s: %s\n", key, strings.Join(values[key], ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}
