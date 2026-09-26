package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
)

const maxRequestedCapabilities = 8

type capabilityPolicyVersion struct {
	sandboxRevision    uint64
	capabilityRevision uint64
}

func (a *Agent) toolCapabilityPolicy(tc messages.ToolCall) (*sandbox.Sandbox, []protocol.CapabilityRequest, capabilityPolicyVersion, error) {
	a.mu.Lock()
	base := a.sandbox
	version := capabilityPolicyVersion{capabilityRevision: a.capabilityRevision}
	if base != nil {
		version.sandboxRevision = base.Revision
		base = base.Snapshot()
	}
	grants := make([]protocol.CapabilityRequest, 0, len(a.capabilityGrants))
	for _, grant := range a.capabilityGrants {
		grants = append(grants, grant)
	}
	revoking := a.capabilityRevoking
	workspace := a.cfg.Workspace
	remoteMCP := a.mcp
	a.mu.Unlock()
	if revoking {
		return nil, nil, version, fmt.Errorf("sandbox capability revocation is pending; inspect /sandbox before requesting further tool work")
	}
	if base == nil {
		return nil, nil, version, fmt.Errorf("sandbox policy is unavailable")
	}
	for _, grant := range grants {
		if err := applyCapability(base, grant); err != nil {
			return nil, nil, version, err
		}
	}

	requested, err := explicitToolCapabilities(tc.Arguments)
	if err != nil {
		return nil, nil, version, err
	}
	requested = append(requested, inferredFileCapability(tc, workspace, base)...)
	needsHostNetwork := tc.Name == "WebFetch" || tc.Name == "WebSearch" || remoteMCP != nil && remoteMCP.RequiresHostNetwork(tc.Name)
	if needsHostNetwork && !base.NetworkAllowed() {
		requested = append(requested, protocol.CapabilityRequest{Kind: "network", Access: "outbound"})
	}
	if len(requested) > maxRequestedCapabilities {
		return nil, nil, version, fmt.Errorf("tool requested more than %d additional capabilities", maxRequestedCapabilities)
	}
	unique := make([]protocol.CapabilityRequest, 0, len(requested))
	seen := map[string]bool{}
	for _, req := range requested {
		canonical, err := canonicalizeCapability(base, req)
		if err != nil {
			return nil, nil, version, err
		}
		key := capabilityKey(canonical)
		if seen[key] {
			continue
		}
		seen[key] = true
		if capabilityGranted(base, canonical) {
			continue
		}
		if a.childState != nil {
			return nil, nil, version, fmt.Errorf("child sandbox capability ceiling does not allow requesting %s", describeCapability(canonical))
		}
		unique = append(unique, canonical)
	}
	sort.Slice(unique, func(i, j int) bool { return capabilityKey(unique[i]) < capabilityKey(unique[j]) })
	return base, unique, version, nil
}

func explicitToolCapabilities(args map[string]any) ([]protocol.CapabilityRequest, error) {
	value, exists := args["requested_capabilities"]
	if !exists || value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("requested_capabilities must be an array: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var requests []protocol.CapabilityRequest
	if err := dec.Decode(&requests); err != nil {
		return nil, fmt.Errorf("requested_capabilities must be a valid capability array: %w", err)
	}
	if len(requests) > maxRequestedCapabilities {
		return nil, fmt.Errorf("requested_capabilities is limited to %d entries", maxRequestedCapabilities)
	}
	return requests, nil
}

func inferredFileCapability(tc messages.ToolCall, workspace string, policy *sandbox.Sandbox) []protocol.CapabilityRequest {
	var rawPath string
	switch tc.Name {
	case "Write", "Edit":
		rawPath = stringArgument(tc.Arguments, "file_path")
	default:
		return nil
	}
	if rawPath == "" {
		return nil
	}
	path := rawPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	if _, err := policy.ResolveWrite(path); err == nil {
		return nil
	}
	dir := filepath.Dir(path)
	return []protocol.CapabilityRequest{{Kind: "directory", Path: dir, Access: "write"}}
}

func canonicalizeCapability(policy *sandbox.Sandbox, req protocol.CapabilityRequest) (protocol.CapabilityRequest, error) {
	switch req.Kind {
	case "directory":
		if req.Access != "write" {
			return req, fmt.Errorf("directory capability access must be write; reads are allowed by default unless explicitly denied")
		}
		if !filepath.IsAbs(req.Path) {
			return req, fmt.Errorf("directory capability requires an absolute path")
		}
		abs, err := filepath.Abs(filepath.Clean(req.Path))
		if err != nil {
			return req, fmt.Errorf("invalid capability directory: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return req, fmt.Errorf("capability directory must already exist: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return req, fmt.Errorf("capability path %q is not an existing directory", req.Path)
		}
		if err := validateGrantDirectory(policy, resolved); err != nil {
			return req, err
		}
		req.Path = filepath.Clean(resolved)
		return req, nil
	case "network":
		if req.Access != "outbound" || req.Path != "" || req.Port != 0 {
			return req, fmt.Errorf("network capability must request only outbound access")
		}
		return req, nil
	case "local_service":
		if req.Address != "localhost" && req.Address != "127.0.0.1" && req.Address != "::1" {
			return req, fmt.Errorf("local service capability must target localhost")
		}
		if req.Direction != "connect" && req.Direction != "listen" {
			return req, fmt.Errorf("local service direction must be connect or listen")
		}
		if req.Protocol == "" {
			req.Protocol = "tcp"
		}
		if req.Protocol != "tcp" && req.Protocol != "udp" {
			return req, fmt.Errorf("local service protocol must be tcp or udp")
		}
		if req.Port == 0 {
			return req, fmt.Errorf("local service capability requires an explicit port")
		}
		req.Address = "localhost"
		return req, nil
	default:
		return req, fmt.Errorf("unknown capability kind %q", req.Kind)
	}
}

func validateGrantDirectory(policy *sandbox.Sandbox, dir string) error {
	if dir == string(filepath.Separator) {
		return fmt.Errorf("the filesystem root cannot be granted as a tool directory")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if samePath(dir, home) {
			return fmt.Errorf("the home directory cannot be granted as a tool directory; request a specific subdirectory")
		}
	}
	snapshot := policy.Snapshot()
	for _, protected := range snapshot.ProtectedDirs {
		if pathContains(dir, protected) || pathContains(protected, dir) {
			return fmt.Errorf("capability directory %q overlaps protected application state", dir)
		}
	}
	for _, denied := range snapshot.DisallowedDirs {
		if pathContains(dir, denied) || pathContains(denied, dir) {
			return fmt.Errorf("capability directory %q overlaps an explicitly disallowed path", dir)
		}
	}
	return nil
}

func capabilityGranted(policy *sandbox.Sandbox, req protocol.CapabilityRequest) bool {
	switch req.Kind {
	case "directory":
		_, err := policy.ResolveWrite(req.Path)
		return err == nil
	case "network":
		return policy.NetworkAllowed()
	case "local_service":
		for _, service := range policy.Snapshot().LocalServices {
			if service.Direction == req.Direction && service.Protocol == req.Protocol && service.Port == req.Port {
				return true
			}
		}
	}
	return false
}

func applyCapability(policy *sandbox.Sandbox, req protocol.CapabilityRequest) error {
	switch req.Kind {
	case "directory":
		if err := validateGrantDirectory(policy, req.Path); err != nil {
			return err
		}
		if req.Access == "write" {
			policy.AddDir(req.Path)
		} else {
			return fmt.Errorf("invalid directory capability access %q", req.Access)
		}
	case "network":
		if req.Access != "outbound" {
			return fmt.Errorf("invalid network capability access %q", req.Access)
		}
		policy.SetAllowNetwork(true)
	case "local_service":
		return policy.AddLocalService(sandbox.LocalService{Direction: req.Direction, Protocol: req.Protocol, Port: req.Port})
	default:
		return fmt.Errorf("unknown capability kind %q", req.Kind)
	}
	return nil
}

func capabilityKey(req protocol.CapabilityRequest) string {
	data, _ := json.Marshal(req)
	return string(data)
}

func (a *Agent) addSessionCapabilities(version capabilityPolicyVersion, requested []protocol.CapabilityRequest) (*sandbox.Sandbox, capabilityPolicyVersion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.capabilityRevoking {
		return nil, version, fmt.Errorf("sandbox capabilities are being revoked")
	}
	if a.sandbox == nil || a.sandbox.Revision != version.sandboxRevision || a.capabilityRevision != version.capabilityRevision {
		return nil, version, fmt.Errorf("sandbox policy changed while capability approval was pending; retry the tool under the current policy")
	}
	if a.turnCtx != nil && a.turnCtx.Err() != nil || a.interruptFlag || a.stop {
		return nil, version, fmt.Errorf("capability approval expired because the operation was interrupted")
	}
	candidate := a.sandbox.Snapshot()
	for _, grant := range a.capabilityGrants {
		if err := applyCapability(candidate, grant); err != nil {
			return nil, version, err
		}
	}
	for _, grant := range requested {
		if err := applyCapability(candidate, grant); err != nil {
			return nil, version, err
		}
	}
	if a.capabilityGrants == nil {
		a.capabilityGrants = map[string]protocol.CapabilityRequest{}
	}
	for _, grant := range requested {
		a.capabilityGrants[capabilityKey(grant)] = grant
	}
	a.capabilityRevision++
	version.capabilityRevision = a.capabilityRevision
	return candidate, version, nil
}

func (a *Agent) approveToolWithCapabilities(tc messages.ToolCall, reason string, base *sandbox.Sandbox, version capabilityPolicyVersion, requests []protocol.CapabilityRequest) (*sandbox.Sandbox, capabilityPolicyVersion, bool, bool) {
	answer := a.requestApprovalDetailed(tc, reason, requests)
	if !answer.approve {
		if !a.capabilityApprovalCurrent(version) {
			return base, version, false, false
		}
		return base, version, false, answer.remember
	}
	if !a.capabilityApprovalCurrent(version) {
		return base, version, false, false
	}
	if len(requests) == 0 {
		return base, version, true, answer.remember
	}
	switch answer.capabilityScope {
	case protocol.CapabilityScopeSession:
		policy, updatedVersion, err := a.addSessionCapabilities(version, requests)
		if err != nil {
			return base, version, false, false
		}
		return policy, updatedVersion, true, false
	case protocol.CapabilityScopeOnce:
		policy := base.Snapshot()
		for _, request := range requests {
			if err := applyCapability(policy, request); err != nil {
				return base, version, false, false
			}
		}
		return policy, version, true, false
	default:
		return base, version, false, false
	}
}

func (a *Agent) capabilityApprovalCurrent(version capabilityPolicyVersion) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.capabilityRevoking || a.sandbox == nil || a.sandbox.Revision != version.sandboxRevision || a.capabilityRevision != version.capabilityRevision {
		return false
	}
	if a.turnCtx != nil && a.turnCtx.Err() != nil {
		return false
	}
	return !a.interruptFlag && !a.stop && !a.closing && !a.closed
}

func (a *Agent) applyRevokeCapabilities(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	wasRevoking := a.capabilityRevoking
	base := a.sandbox
	if base != nil {
		base = base.Snapshot()
	}
	grants := make(map[string]protocol.CapabilityRequest, len(a.capabilityGrants))
	for key, grant := range a.capabilityGrants {
		grants[key] = grant
	}
	if len(grants) == 0 && !wasRevoking {
		a.mu.Unlock()
		return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
	}
	if !wasRevoking {
		a.capabilityRevoking = true
		// Invalidate all outstanding approval prompts at the same moment that
		// dispatch is frozen. The revoking bit stays set if any cleanup fails.
		a.capabilityRevision++
	}
	turnCancel, compactCancel := a.turnCancel, a.compactCancel
	resources, oldMCP, supervisor := a.resources, a.mcp, a.supervisor
	a.mu.Unlock()

	targets := map[string]bool{}
	if cmd.RevokeCapabilities.All {
		for key := range grants {
			targets[key] = true
		}
	} else {
		if base == nil {
			if !wasRevoking {
				a.resetCapabilityRevoking()
			}
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, "sandbox policy is unavailable")
		}
		policy := base.Snapshot()
		for _, grant := range grants {
			if err := applyCapability(policy, grant); err != nil {
				if !wasRevoking {
					a.resetCapabilityRevoking()
				}
				return a.rejectedReceipt(cmd, protocol.ErrorInternal, "rebuild current session sandbox policy: "+err.Error())
			}
		}
		for _, requested := range cmd.RevokeCapabilities.Capabilities {
			canonical, err := canonicalizeCapability(policy, requested)
			if err != nil {
				if !wasRevoking {
					a.resetCapabilityRevoking()
				}
				return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
			}
			key := capabilityKey(canonical)
			if _, ok := grants[key]; !ok {
				if !wasRevoking {
					a.resetCapabilityRevoking()
				}
				return a.rejectedReceipt(cmd, protocol.ErrorNotFound, "the requested capability is not granted in this session")
			}
			targets[key] = true
		}
	}

	// Close every admission gate before interrupting work. ProcessManager keeps
	// starts blocked until every old process has exited; the MCP manager is
	// closed and replaced only after the active turn has released its leases.
	var revokeErr error
	if resources != nil && resources.Processes != nil {
		if err := resources.Processes.StopAll(); err != nil {
			revokeErr = fmt.Errorf("stop background processes: %w", err)
		}
	}
	if turnCancel != nil {
		turnCancel()
	}
	if compactCancel != nil {
		compactCancel()
	}
	if oldMCP != nil {
		if err := oldMCP.Close(); err != nil {
			revokeErr = errors.Join(revokeErr, fmt.Errorf("close MCP connections: %w", err))
		}
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if supervisor != nil {
		if err := supervisor.revokeCapabilities(waitCtx); err != nil {
			revokeErr = errors.Join(revokeErr, fmt.Errorf("stop descendant sessions: %w", err))
		}
	}
	if err := waitGroupContext(waitCtx, &a.turnWG); err != nil {
		revokeErr = errors.Join(revokeErr, fmt.Errorf("wait for active turn: %w", err))
	}
	if err := waitGroupContext(waitCtx, &a.operationWG); err != nil {
		revokeErr = errors.Join(revokeErr, fmt.Errorf("wait for active runtime operation: %w", err))
	}
	if base != nil && base.ExternalExecutionPossible() {
		revokeErr = errors.Join(revokeErr, errors.New("a local process may have descendants retaining the previous Seatbelt policy; revocation cannot be confirmed until the session is restarted and those processes are independently terminated"))
	}
	if revokeErr != nil {
		// Fail closed. Dispatch remains disabled and process starts remain gated.
		// Rejoining managed resources cannot clear the session-sticky witness for
		// detached descendants; that case requires independent cleanup and a new
		// session before revocation can be confirmed.
		a.emitStatus("sandbox capability revocation incomplete: %v", revokeErr)
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, "revocation is incomplete; new tool dispatch remains blocked: "+revokeErr.Error())
	}

	a.settingsCommitMu.Lock()
	a.mu.Lock()
	if a.sandbox == nil {
		a.mu.Unlock()
		a.settingsCommitMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, "sandbox policy disappeared during revocation; new tool dispatch remains blocked")
	}
	replacement := mcp.NewManager()
	replacement.SetSandbox(a.sandbox.Snapshot())
	replacement.RegisterTools(a.registry)
	a.mcp = replacement
	for name := range a.deferTools {
		if strings.HasPrefix(name, "mcp__") {
			delete(a.deferTools, name)
			delete(a.discovered, name)
		}
	}
	for key := range targets {
		delete(a.capabilityGrants, key)
	}
	a.capabilityRevision++
	a.capabilityRevoking = false
	a.catalogVersion++
	a.mu.Unlock()
	a.settingsCommitMu.Unlock()
	if resources != nil && resources.Processes != nil {
		resources.Processes.ResumeAfterRevocation()
	}
	a.emitStatus("session sandbox capabilities revoked; MCP connections were closed; run /reload to reconnect configured servers")
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}

func (a *Agent) resetCapabilityRevoking() {
	a.mu.Lock()
	a.capabilityRevoking = false
	a.mu.Unlock()
}

func waitGroupContext(ctx context.Context, group interface{ Wait() }) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func formatCapabilityRequests(requests []protocol.CapabilityRequest) string {
	lines := []string{"Additional sandbox access requested:"}
	for _, req := range requests {
		switch req.Kind {
		case "directory":
			lines = append(lines, fmt.Sprintf("- %s directory: %s", req.Access, req.Path))
		case "network":
			lines = append(lines, "- outbound TCP/UDP to non-local network destinations (Unix sockets remain blocked)")
		case "local_service":
			lines = append(lines, fmt.Sprintf("- %s %s localhost service on port %d; Seatbelt may match other host-local addresses on that port", req.Direction, req.Protocol, req.Port))
		}
	}
	lines = append(lines, "Choose whether this access lasts for this call or the current session.")
	return strings.Join(lines, "\n")
}

func describeCapability(req protocol.CapabilityRequest) string {
	switch req.Kind {
	case "directory":
		return fmt.Sprintf("%s directory %s", req.Access, req.Path)
	case "network":
		return "outbound network"
	case "local_service":
		return fmt.Sprintf("%s %s localhost service port %d", req.Direction, req.Protocol, req.Port)
	default:
		return req.Kind
	}
}

func pathContains(root, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func samePath(a, b string) bool {
	clean := func(path string) string {
		abs, err := filepath.Abs(path)
		if err != nil {
			return filepath.Clean(path)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return filepath.Clean(resolved)
		}
		return filepath.Clean(abs)
	}
	return clean(a) == clean(b)
}

func stringArgument(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return value
}
