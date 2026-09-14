package config

// This file contains the small amount of provenance and project-trust state
// that accompanies a resolved Config.  It is intentionally separate from the
// JSON-facing Config fields: provenance is runtime evidence, never something
// a project settings file can claim for itself.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccdp/internal/atomicfile"
	"ccdp/internal/hooks"
	"ccdp/internal/mcp"
	"ccdp/internal/sandbox"
)

// Source identifies the layer which supplied a resolved field.  The values
// are ordered from least to most authoritative for diagnostics only; callers
// must not infer permission from the order (project settings are constrained
// separately by ApplyProjectSettings).
type Source string

const (
	SourceDefault      Source = "default"
	SourceUserFile     Source = "user-file"
	SourceProject      Source = "project"
	SourceProjectLocal Source = "project-local"
	SourceEnvironment  Source = "environment"
	SourceCLI          Source = "cli"
	SourceSession      Source = "session"
)

// FieldProvenance is the auditable origin of one resolved field.
type FieldProvenance struct {
	Source      Source `json:"source"`
	Explicit    bool   `json:"explicit"`
	Trusted     bool   `json:"trusted,omitempty"`
	Path        string `json:"path,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ProvenanceReport is a copy safe report suitable for ConfigSummary, doctor,
// and protocol responses.  Mutating it never mutates a Config.
type ProvenanceReport struct {
	Fields             map[string]FieldProvenance `json:"fields"`
	ProjectRoot        string                     `json:"project_root,omitempty"`
	ProjectTrusted     bool                       `json:"project_trusted"`
	ProjectFingerprint string                     `json:"project_fingerprint,omitempty"`
}

type provenanceState struct {
	mu                 sync.RWMutex
	fields             map[string]FieldProvenance
	projectRoot        string
	projectTrusted     bool
	projectFingerprint string
}

var configFieldNames = []string{
	"api_key", "base_url", "model", "workspace", "providers", "reasoning_effort", "verbosity",
	"permission_mode", "always_allow", "always_deny", "system_prompt",
	"context_window", "compact_threshold", "max_result_size_chars",
	"keep_after_compact", "bash_timeout_seconds", "max_turns",
	"max_budget_usd", "max_reply_tokens", "fallback_model",
	"max_tool_output_chars_per_turn", "enable_guardian", "enable_memory",
	"sandbox_mode", "max_parallel_tools", "sandbox_limits",
	"sandbox_allow_network", "additional_directories", "disallowed_directories",
	"hooks", "enable_web_tools", "tools", "mcp_servers", "pricing", "session_dir",
	"verbose", "debug", "session_id", "no_session_persistence",
}

func newProvenance() *provenanceState {
	p := &provenanceState{fields: make(map[string]FieldProvenance, len(configFieldNames))}
	for _, name := range configFieldNames {
		p.fields[name] = FieldProvenance{Source: SourceDefault}
	}
	return p
}

func (p *provenanceState) clone() *provenanceState {
	if p == nil {
		return newProvenance()
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := &provenanceState{
		fields:             make(map[string]FieldProvenance, len(p.fields)),
		projectRoot:        p.projectRoot,
		projectTrusted:     p.projectTrusted,
		projectFingerprint: p.projectFingerprint,
	}
	for k, v := range p.fields {
		out.fields[k] = v
	}
	return out
}

func (p *provenanceState) set(field string, value FieldProvenance) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.fields == nil {
		p.fields = make(map[string]FieldProvenance)
	}
	p.fields[field] = value
	p.mu.Unlock()
}

func (p *provenanceState) setProject(root string, trusted bool, fingerprint string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.projectRoot, p.projectTrusted, p.projectFingerprint = root, trusted, fingerprint
	p.mu.Unlock()
}

func (p *provenanceState) report() ProvenanceReport {
	if p == nil {
		p = newProvenance()
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	fields := make(map[string]FieldProvenance, len(p.fields))
	for k, v := range p.fields {
		fields[k] = v
	}
	return ProvenanceReport{Fields: fields, ProjectRoot: p.projectRoot,
		ProjectTrusted: p.projectTrusted, ProjectFingerprint: p.projectFingerprint}
}

func (p *provenanceState) source(field string) Source {
	if p == nil {
		return SourceDefault
	}
	p.mu.RLock()
	v, ok := p.fields[field]
	p.mu.RUnlock()
	if !ok || v.Source == "" {
		return SourceDefault
	}
	return v.Source
}

// SourceReport returns a copy of the resolved field provenance.
func (c Config) SourceReport() ProvenanceReport {
	return c.provenance.report()
}

// SourceOf reports the layer that resolved field. Unknown fields are treated
// as built-in defaults rather than being reported as user supplied.
func (c Config) SourceOf(field string) Source { return c.provenance.source(field) }

// SourcePath returns the user configuration file used to resolve this value.
// It is read-only metadata for diagnostics; callers must not infer permission
// from the path.
func (c Config) SourcePath() string { return c.sourcePath }

// IsExplicit reports whether the field appeared in any source file or
// override. An explicit empty, false, or zero value is still explicit.
func (c Config) IsExplicit(field string) bool {
	if c.provenance == nil {
		return false
	}
	c.provenance.mu.RLock()
	v := c.provenance.fields[field]
	c.provenance.mu.RUnlock()
	return v.Explicit
}

// ApplyCLIOverride applies one command-line value with presence semantics and
// records SourceCLI provenance.  The caller invokes this only for flags that
// were actually present; an explicit false, zero, or empty slice therefore
// remains a real override instead of being mistaken for an unset flag.
//
// Keeping the conversion in config makes CLI precedence auditable without
// exposing the internal provenance state or allowing a project file to claim
// CLI authority.
func (c *Config) ApplyCLIOverride(field string, value any) error {
	if c == nil {
		return errors.New("config: nil config")
	}
	field = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(field)), "ccdp_")
	setString := func(dst *string) error {
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("config: CLI %s must be a string", field)
		}
		*dst = v
		return nil
	}
	setBool := func(dst *bool) error {
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("config: CLI %s must be a bool", field)
		}
		*dst = v
		return nil
	}
	setInt := func(dst *int) error {
		v, ok := value.(int)
		if !ok {
			return fmt.Errorf("config: CLI %s must be an int", field)
		}
		*dst = v
		return nil
	}
	setFloat := func(dst *float64) error {
		v, ok := value.(float64)
		if !ok {
			return fmt.Errorf("config: CLI %s must be a float64", field)
		}
		*dst = v
		return nil
	}
	setStrings := func(dst *[]string) error {
		v, ok := value.([]string)
		if !ok {
			return fmt.Errorf("config: CLI %s must be []string", field)
		}
		*dst = append([]string(nil), v...)
		return nil
	}
	var err error
	switch field {
	case "api_key":
		err = setString(&c.APIKey)
	case "base_url":
		err = setString(&c.BaseURL)
	case "reasoning_effort":
		err = setString(&c.ReasoningEffort)
	case "verbosity":
		err = setString(&c.Verbosity)
	case "model":
		err = setString(&c.Model)
	case "workspace":
		err = setString(&c.Workspace)
	case "permission_mode":
		err = setString(&c.PermissionMode)
	case "always_allow":
		err = setStrings(&c.AlwaysAllow)
	case "always_deny":
		err = setStrings(&c.AlwaysDeny)
	case "system_prompt":
		err = setString(&c.SystemPrompt)
	case "context_window":
		err = setInt(&c.ContextWindow)
	case "compact_threshold":
		err = setFloat(&c.CompactThreshold)
	case "max_result_size_chars":
		err = setInt(&c.MaxResultSizeChars)
	case "keep_after_compact":
		err = setInt(&c.KeepAfterCompact)
	case "bash_timeout_seconds":
		err = setInt(&c.BashTimeoutSeconds)
	case "max_turns":
		err = setInt(&c.MaxTurns)
	case "max_budget_usd":
		err = setFloat(&c.MaxBudgetUSD)
	case "verbose":
		err = setBool(&c.Verbose)
	case "debug":
		err = setBool(&c.Debug)
	case "max_reply_tokens":
		err = setInt(&c.MaxReplyTokens)
	case "fallback_model":
		err = setString(&c.FallbackModel)
	case "max_tool_output_chars_per_turn":
		err = setInt(&c.MaxToolOutputCharsPerTurn)
	case "enable_guardian":
		var v bool
		err = setBool(&v)
		if err == nil {
			c.EnableGuardian = cloneBool(&v)
		}
	case "enable_memory":
		var v bool
		err = setBool(&v)
		if err == nil {
			c.EnableMemory = cloneBool(&v)
		}
	case "sandbox_mode":
		err = setString(&c.SandboxMode)
	case "max_parallel_tools":
		err = setInt(&c.MaxParallelTools)
	case "sandbox_allow_network":
		err = setBool(&c.SandboxAllowNetwork)
	case "additional_directories":
		err = setStrings(&c.AdditionalDirectories)
	case "disallowed_directories":
		err = setStrings(&c.DisallowedDirectories)
	case "enable_web_tools":
		var v bool
		err = setBool(&v)
		if err == nil {
			c.EnableWebTools = cloneBool(&v)
		}
	case "session_dir":
		err = setString(&c.SessionDir)
	case "session_id":
		err = setString(&c.SessionID)
	case "no_session_persistence":
		err = setBool(&c.NoSessionPersistence)
	default:
		return fmt.Errorf("config: unsupported CLI override %q", field)
	}
	if err != nil {
		return err
	}
	c.markSource(field, SourceCLI, "cli:"+field, false)
	return nil
}

func (c *Config) ensureProvenance() {
	if c.provenance == nil {
		c.provenance = newProvenance()
	}
}

func (c *Config) markSource(field string, source Source, path string, trusted bool) {
	c.ensureProvenance()
	c.provenance.set(field, FieldProvenance{Source: source, Explicit: true, Path: path, Trusted: trusted})
}

// applyConfigFields overlays exactly the JSON keys present in raw. A decoded
// zero value is therefore still an override; callers must not use ordinary Go
// zero-value merging for configuration files.
func applyConfigFields(dst, src *Config, raw map[string]json.RawMessage, source Source, path string, trusted bool) {
	if dst == nil || src == nil {
		return
	}
	for field := range raw {
		switch field {
		case "api_key":
			dst.APIKey = src.APIKey
		case "base_url":
			dst.BaseURL = src.BaseURL
		case "reasoning_effort":
			dst.ReasoningEffort = src.ReasoningEffort
		case "verbosity":
			dst.Verbosity = src.Verbosity
		case "model":
			dst.Model = src.Model
		case "workspace":
			dst.Workspace = src.Workspace
		case "providers":
			dst.Providers = cloneProviders(src.Providers)
		case "permission_mode":
			dst.PermissionMode = src.PermissionMode
		case "always_allow":
			dst.AlwaysAllow = append([]string(nil), src.AlwaysAllow...)
		case "always_deny":
			dst.AlwaysDeny = append([]string(nil), src.AlwaysDeny...)
		case "system_prompt":
			dst.SystemPrompt = src.SystemPrompt
		case "context_window":
			dst.ContextWindow = src.ContextWindow
		case "compact_threshold":
			dst.CompactThreshold = src.CompactThreshold
		case "max_result_size_chars":
			dst.MaxResultSizeChars = src.MaxResultSizeChars
		case "keep_after_compact":
			dst.KeepAfterCompact = src.KeepAfterCompact
		case "bash_timeout_seconds":
			dst.BashTimeoutSeconds = src.BashTimeoutSeconds
		case "max_turns":
			dst.MaxTurns = src.MaxTurns
		case "max_budget_usd":
			dst.MaxBudgetUSD = src.MaxBudgetUSD
		case "verbose":
			dst.Verbose = src.Verbose
		case "debug":
			dst.Debug = src.Debug
		case "max_reply_tokens":
			dst.MaxReplyTokens = src.MaxReplyTokens
		case "fallback_model":
			dst.FallbackModel = src.FallbackModel
		case "max_tool_output_chars_per_turn":
			dst.MaxToolOutputCharsPerTurn = src.MaxToolOutputCharsPerTurn
		case "enable_guardian":
			dst.EnableGuardian = cloneBool(src.EnableGuardian)
		case "enable_memory":
			dst.EnableMemory = cloneBool(src.EnableMemory)
		case "sandbox_mode":
			dst.SandboxMode = src.SandboxMode
		case "max_parallel_tools":
			dst.MaxParallelTools = src.MaxParallelTools
		case "sandbox_limits":
			dst.SandboxLimits = cloneLimits(src.SandboxLimits)
		case "sandbox_allow_network":
			dst.SandboxAllowNetwork = src.SandboxAllowNetwork
		case "additional_directories":
			dst.AdditionalDirectories = append([]string(nil), src.AdditionalDirectories...)
		case "disallowed_directories":
			dst.DisallowedDirectories = append([]string(nil), src.DisallowedDirectories...)
		case "hooks":
			dst.Hooks = cloneHooks(src.Hooks)
		case "enable_web_tools":
			dst.EnableWebTools = cloneBool(src.EnableWebTools)
		case "tools":
			dst.Tools = append([]ToolSpec(nil), src.Tools...)
			for i := range dst.Tools {
				dst.Tools[i].InputSchema = cloneAnyMap(dst.Tools[i].InputSchema)
			}
		case "mcp_servers":
			dst.MCPServers = cloneMCPServers(src.MCPServers)
		case "pricing":
			dst.Pricing = clonePricing(src.Pricing)
		case "session_dir":
			dst.SessionDir = src.SessionDir
		case "session_id":
			dst.SessionID = src.SessionID
		case "no_session_persistence":
			dst.NoSessionPersistence = src.NoSessionPersistence
		}
		// Unknown keys are intentionally ignored by Config's JSON decoder but
		// do not appear in the report either.
		for _, known := range configFieldNames {
			if known == field {
				dst.markSource(field, source, path, trusted)
				break
			}
		}
	}
}

func cloneBool(src *bool) *bool {
	if src == nil {
		return nil
	}
	v := *src
	return &v
}

func cloneLimits(src *sandbox.Limits) *sandbox.Limits {
	if src == nil {
		return nil
	}
	v := *src
	return &v
}

// Clone returns a value copy whose mutable provenance state is independent.
// Agent step snapshots use this instead of a plain struct copy.
func (c Config) Clone() Config {
	c.provenance = c.provenance.clone()
	c.EnableGuardian = cloneBool(c.EnableGuardian)
	c.EnableMemory = cloneBool(c.EnableMemory)
	c.EnableWebTools = cloneBool(c.EnableWebTools)
	c.AlwaysAllow = append([]string(nil), c.AlwaysAllow...)
	c.AlwaysDeny = append([]string(nil), c.AlwaysDeny...)
	c.AdditionalDirectories = append([]string(nil), c.AdditionalDirectories...)
	c.DisallowedDirectories = append([]string(nil), c.DisallowedDirectories...)
	c.Tools = append([]ToolSpec(nil), c.Tools...)
	for i := range c.Tools {
		c.Tools[i].InputSchema = cloneAnyMap(c.Tools[i].InputSchema)
	}
	c.Providers = cloneProviders(c.Providers)
	c.MCPServers = cloneMCPServers(c.MCPServers)
	c.Pricing = clonePricing(c.Pricing)
	c.Hooks = cloneHooks(c.Hooks)
	if c.SandboxLimits != nil {
		limits := *c.SandboxLimits
		c.SandboxLimits = &limits
	}
	return c
}

func cloneAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = cloneAnyValue(v)
	}
	return dst
}

// cloneAnyValue covers the JSON-shaped values accepted by tool schemas. It
// deliberately preserves scalar concrete types while recursively separating
// nested maps and slices; a plain map copy would let a child/candidate mutate
// the parent's schema through an inner container.
func cloneAnyValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneAnyMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneAnyValue(item)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for key, item := range v {
			out[key] = item
		}
		return out
	case []string:
		return append([]string(nil), v...)
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i, item := range v {
			out[i] = cloneAnyMap(item)
		}
		return out
	default:
		return value
	}
}

func cloneProviders(src map[string]ProviderConfig) map[string]ProviderConfig {
	dst := make(map[string]ProviderConfig, len(src))
	for k, v := range src {
		v.Models = append([]string(nil), v.Models...)
		dst[k] = v
	}
	return dst
}

func cloneMCPServers(src map[string]mcp.ServerConfig) map[string]mcp.ServerConfig {
	dst := make(map[string]mcp.ServerConfig, len(src))
	for k, v := range src {
		v.Args = append([]string(nil), v.Args...)
		v.Env = cloneStringMap(v.Env)
		dst[k] = v
	}
	return dst
}

func clonePricing(src map[string]Pricing) map[string]Pricing {
	dst := make(map[string]Pricing, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneHooks(src hooks.Config) hooks.Config {
	if src == nil {
		return nil
	}
	dst := make(hooks.Config, len(src))
	for k, v := range src {
		dst[k] = append([]hooks.HookSpec(nil), v...)
	}
	return dst
}

// ProjectTrust is the externally stored approval for executable project
// configuration. It intentionally contains only a normalized root and the
// executable-config fingerprint; no project file can mark itself trusted.
type ProjectTrust struct {
	Root        string    `json:"root"`
	Fingerprint string    `json:"fingerprint"`
	ApprovedAt  time.Time `json:"approved_at"`
}

// TrustStore persists explicit project approvals outside the project tree.
// Construct one with a test-specific path in tests; DefaultTrustStore is used
// by a CLI command when the user explicitly authorizes a project.
type TrustStore struct {
	Path string
}

var trustStoreMu sync.Mutex

func NewTrustStore(path string) *TrustStore { return &TrustStore{Path: path} }

func DefaultTrustStore() *TrustStore {
	home, _ := os.UserHomeDir()
	return NewTrustStore(filepath.Join(home, ".ccdp", "project-trust.json"))
}

func NormalizeProjectRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("config: project root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("config: normalize project root: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("config: resolve project root: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return "", fmt.Errorf("config: project root: %w", err)
	}
	return filepath.Clean(abs), nil
}

// ProjectExecutableFingerprint hashes project settings whose application can
// grant execution/network privilege. Map encoding is deterministic in
// encoding/json; slices retain their declared order because order can affect
// hook/tool precedence. Keep the structured hook fields here instead of using
// hooks.Config's compatibility JSON method, which intentionally emits only
// command strings and would allow a matcher or timeout change to evade trust
// invalidation. Permission grants and network enables are included as well:
// changing one after authorization must invalidate the authorization.
func ProjectExecutableFingerprint(root string, cfg Config) (string, error) {
	normalized, err := NormalizeProjectRoot(root)
	if err != nil {
		return "", err
	}
	payload := struct {
		Root                  string                      `json:"root"`
		Hooks                 map[string][]hooks.HookSpec `json:"hooks"`
		Tools                 []ToolSpec                  `json:"tools"`
		MCPServers            map[string]mcp.ServerConfig `json:"mcp_servers"`
		AlwaysAllow           []string                    `json:"always_allow"`
		EnableWebTools        *bool                       `json:"enable_web_tools"`
		AdditionalDirectories []string                    `json:"additional_directories"`
		SandboxAllowNetwork   bool                        `json:"sandbox_allow_network"`
	}{
		Root:                  normalized,
		Hooks:                 cloneHookSpecs(cfg.Hooks),
		Tools:                 cloneToolsForFingerprint(cfg.Tools),
		MCPServers:            cloneMCPServers(cfg.MCPServers),
		AlwaysAllow:           append([]string(nil), cfg.AlwaysAllow...),
		EnableWebTools:        cloneBool(cfg.EnableWebTools),
		AdditionalDirectories: append([]string(nil), cfg.AdditionalDirectories...),
		SandboxAllowNetwork:   cfg.SandboxAllowNetwork,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("config: project fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func cloneHookSpecs(src hooks.Config) map[string][]hooks.HookSpec {
	if src == nil {
		return nil
	}
	dst := make(map[string][]hooks.HookSpec, len(src))
	for event, specs := range src {
		dst[event] = append([]hooks.HookSpec(nil), specs...)
	}
	return dst
}

func cloneToolsForFingerprint(src []ToolSpec) []ToolSpec {
	out := append([]ToolSpec(nil), src...)
	for i := range out {
		out[i].InputSchema = cloneAnyMap(out[i].InputSchema)
	}
	return out
}

func (s *TrustStore) load() (map[string]ProjectTrust, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return map[string]ProjectTrust{}, nil
	}
	info, statErr := os.Stat(s.Path)
	if statErr == nil && info.Size() > 1<<20 {
		return nil, errors.New("config: trust store exceeds 1 MiB")
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]ProjectTrust{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read trust store: %w", err)
	}
	var out map[string]ProjectTrust
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("config: parse trust store: %w", err)
	}
	if out == nil {
		out = map[string]ProjectTrust{}
	}
	return out, nil
}

func (s *TrustStore) save(records map[string]ProjectTrust) error {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return errors.New("config: trust store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("config: create trust store: %w", err)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(s.Path, data, 0o600)
}

// withLock serializes trust-store read/modify/write transactions across both
// goroutines and processes.  The in-process mutex below protects callers in
// this process; the sidecar advisory lock prevents a second ccdp process from
// loading an old map and silently overwriting a concurrent authorization.
func (s *TrustStore) withLock(create bool, fn func() error) error {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return errors.New("config: trust store path is required")
	}
	dir := filepath.Dir(s.Path)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("config: create trust store directory: %w", err)
		}
	} else {
		// A missing store cannot authorize anything. Reading that state without
		// creating a directory or lock file is side-effect free and fail-closed.
		if _, err := os.Stat(s.Path); os.IsNotExist(err) {
			return fn()
		} else if err != nil {
			return fmt.Errorf("config: inspect trust store: %w", err)
		}
	}
	lock, err := atomicfile.AcquireLock(s.Path + ".lock")
	if err != nil {
		return fmt.Errorf("config: lock trust store: %w", err)
	}
	defer func() { _ = lock.Close() }()
	return fn()
}

// AuthorizeProject is the explicit CLI/user action that enables executable
// project settings. It never edits files below root and records only a
// fingerprint in the external trust store.
func (s *TrustStore) AuthorizeProject(root string, cfg Config) (ProjectTrust, error) {
	trustStoreMu.Lock()
	defer trustStoreMu.Unlock()
	var record ProjectTrust
	err := s.withLock(true, func() error {
		normalized, err := NormalizeProjectRoot(root)
		if err != nil {
			return err
		}
		fingerprint, err := ProjectExecutableFingerprint(normalized, cfg)
		if err != nil {
			return err
		}
		record = ProjectTrust{Root: normalized, Fingerprint: fingerprint, ApprovedAt: time.Now().UTC()}
		records, err := s.load()
		if err != nil {
			return err
		}
		records[normalized] = record
		return s.save(records)
	})
	if err != nil {
		return ProjectTrust{}, err
	}
	return record, nil
}

// RevokeProject removes an explicit approval. Missing records are harmless.
func (s *TrustStore) RevokeProject(root string) error {
	trustStoreMu.Lock()
	defer trustStoreMu.Unlock()
	return s.withLock(true, func() error {
		normalized, err := NormalizeProjectRoot(root)
		if err != nil {
			return err
		}
		records, err := s.load()
		if err != nil {
			return err
		}
		delete(records, normalized)
		return s.save(records)
	})
}

// Check reports whether root is currently approved for exactly cfg's
// executable configuration. Changed hooks/tools/MCP invalidate approval.
func (s *TrustStore) Check(root string, cfg Config) (bool, ProjectTrust, error) {
	trustStoreMu.Lock()
	defer trustStoreMu.Unlock()
	var trusted bool
	var record ProjectTrust
	err := s.withLock(false, func() error {
		normalized, err := NormalizeProjectRoot(root)
		if err != nil {
			return err
		}
		fingerprint, err := ProjectExecutableFingerprint(normalized, cfg)
		if err != nil {
			return err
		}
		records, err := s.load()
		if err != nil {
			return err
		}
		record, _ = records[normalized]
		trusted = record.Root == normalized && record.Fingerprint == fingerprint
		return nil
	})
	if err != nil {
		return false, ProjectTrust{}, err
	}
	return trusted, record, nil
}
