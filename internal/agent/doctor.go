package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
)

// ConfigSummary renders the runtime configuration for /config.
func (a *Agent) ConfigSummary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "model:         %s\n", a.cfg.Model)
	fmt.Fprintf(&sb, "base_url:      %s\n", a.cfg.BaseURL)
	fmt.Fprintf(&sb, "api_key:       %s\n", maskKey(a.cfg.APIKey))
	fmt.Fprintf(&sb, "permission:    %s\n", a.perms.CurrentMode())
	fmt.Fprintf(&sb, "sandbox:       %s", a.sandbox.Mode)
	if a.sandbox.Limits != nil {
		sb.WriteString(" (limits: on)")
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "plan mode:     %v\n", a.inPlanMode())
	fmt.Fprintf(&sb, "workspace:     %s\n", a.cfg.Workspace)
	fmt.Fprintf(&sb, "context_window:%d\n", a.cfg.ContextWindow)
	fmt.Fprintf(&sb, "compact_thr:   %.0f%%\n", a.cfg.CompactThreshold*100)
	fmt.Fprintf(&sb, "max_turns:     %d\n", a.cfg.MaxTurns)
	if a.cfg.MaxBudgetUSD > 0 {
		fmt.Fprintf(&sb, "max_budget:    $%.2f\n", a.cfg.MaxBudgetUSD)
	}
	fmt.Fprintf(&sb, "tools:         %d\n", len(a.registry.Names()))
	fmt.Fprintf(&sb, "plugins:       %d\n", len(a.host.Names()))
	fmt.Fprintf(&sb, "mcp servers:   %d\n", len(a.mcp.Names()))
	fmt.Fprintf(&sb, "skills:        %d\n", len(a.skills.All()))
	fmt.Fprintf(&sb, "config file:   %s", config.ConfigPath())
	return sb.String()
}

func maskKey(k string) string {
	if k == "" {
		return "(not set)"
	}
	if len(k) <= 8 {
		return "••••"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// Doctor runs environment diagnostics for /doctor.
func (a *Agent) Doctor() string {
	var sb strings.Builder
	ok := func(name string, pass bool, detail string) {
		status := "✓"
		if !pass {
			status = "✗"
		}
		if detail != "" {
			fmt.Fprintf(&sb, "%s %s — %s\n", status, name, detail)
		} else {
			fmt.Fprintf(&sb, "%s %s\n", status, name)
		}
	}

	// Workspace.
	st, err := os.Stat(a.cfg.Workspace)
	ok("workspace exists", err == nil && st.IsDir(), a.cfg.Workspace)

	// Writable.
	tmp := filepath.Join(a.cfg.Workspace, ".ccdp-write-test")
	ok("workspace writable", os.WriteFile(tmp, []byte("x"), 0o600) == nil, "")
	_ = os.Remove(tmp)

	// Session dir writable.
	ok("session dir writable", os.MkdirAll(a.cfg.SessionDir, 0o755) == nil, a.cfg.SessionDir)

	// API key.
	ok("api key configured", a.cfg.APIKey != "", "")

	// Base URL reachable (just format sanity, not a live ping).
	ok("base_url set", a.cfg.BaseURL != "", a.cfg.BaseURL)

	// Config file.
	ok("config file present", fileExists(config.ConfigPath()), config.ConfigPath())

	// Sandbox.
	ok("sandbox active", a.sandbox != nil, string(a.sandbox.Mode))

	// Tools.
	names := a.registry.Names()
	ok("tools loaded", len(names) > 0, fmt.Sprintf("%d tools", len(names)))

	// MCP.
	if n := len(a.mcp.Names()); n > 0 {
		ok("mcp servers", true, fmt.Sprintf("%d connected", n))
	} else {
		ok("mcp servers", true, "none configured")
	}

	// Skills.
	ok("skills loaded", true, fmt.Sprintf("%d skills", len(a.skills.All())))

	return strings.TrimRight(sb.String(), "\n")
}

// PermissionsInfo renders the current permission state for /permissions.
func (a *Agent) PermissionsInfo() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Permission mode: %s\n", a.perms.CurrentMode())
	fmt.Fprintf(&sb, "Always allow:\n")
	if len(a.cfg.AlwaysAllow) == 0 {
		sb.WriteString("  (none)\n")
	}
	for _, r := range a.cfg.AlwaysAllow {
		sb.WriteString("  - " + r + "\n")
	}
	fmt.Fprintf(&sb, "Always deny:\n")
	if len(a.cfg.AlwaysDeny) == 0 {
		sb.WriteString("  (none)\n")
	}
	for _, r := range a.cfg.AlwaysDeny {
		sb.WriteString("  - " + r + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// AddAlwaysRule persists an allow/deny rule to the user config (Claude Code's
// /permissions allow|deny) and applies it immediately.
func (a *Agent) AddAlwaysRule(kind, rule string) error {
	if rule == "" {
		return fmt.Errorf("rule is required")
	}
	switch kind {
	case "allow":
		a.cfg.AlwaysAllow = appendUnique(a.cfg.AlwaysAllow, rule)
	case "deny":
		a.cfg.AlwaysDeny = appendUnique(a.cfg.AlwaysDeny, rule)
	default:
		return fmt.Errorf("kind must be allow or deny")
	}
	a.perms.SetPolicy(permissions.Policy{AlwaysAllow: a.cfg.AlwaysAllow, AlwaysDeny: a.cfg.AlwaysDeny})
	a.emitStatus("added %s rule %q (persisted to config)", kind, rule)
	return a.cfg.Save()
}

// RemoveAlwaysRule removes an allow/deny rule and persists the change.
func (a *Agent) RemoveAlwaysRule(kind, rule string) error {
	switch kind {
	case "allow":
		a.cfg.AlwaysAllow = removeStr(a.cfg.AlwaysAllow, rule)
	case "deny":
		a.cfg.AlwaysDeny = removeStr(a.cfg.AlwaysDeny, rule)
	default:
		return fmt.Errorf("kind must be allow or deny")
	}
	a.perms.SetPolicy(permissions.Policy{AlwaysAllow: a.cfg.AlwaysAllow, AlwaysDeny: a.cfg.AlwaysDeny})
	a.emitStatus("removed %s rule %q", kind, rule)
	return a.cfg.Save()
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func removeStr(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
