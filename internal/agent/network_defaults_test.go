package agent

import (
	"context"
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
	"ccdp/internal/tools"
)

func TestReopenSessionNetworkPolicy(t *testing.T) {
	cfg := testPersistenceConfig(t)
	for _, tc := range []struct {
		name            string
		outbound, local bool
	}{
		{"online", true, true}, {"offline", false, false}, {"local-only", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := cfg.Clone()
			initial.NetworkAccess, initial.LocalDevelopmentNetwork = tc.outbound, tc.local
			writer, err := openSessionPersistence(&initial, tc.name, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			snap, err := archive.Load(cfg.SessionDir, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			base := cfg.Clone()
			base.NetworkAccess, base.LocalDevelopmentNetwork = true, true
			resumed, err := ResolveResumeConfig(&base, snap)
			if err != nil || resumed.NetworkAccess != tc.outbound || resumed.LocalDevelopmentNetwork != tc.local {
				t.Fatalf("reopened with outbound=%v local=%v err=%v", resumed.NetworkAccess, resumed.LocalDevelopmentNetwork, err)
			}
		})
	}
}

func TestLegacyCreationWithoutNetworkPolicy(t *testing.T) {
	cfg := testPersistenceConfig(t)
	const id = "legacy"
	metadata := &session.SettingsChanged{Revision: 1, Settings: session.Settings{Workspace: cfg.Workspace, Model: cfg.Model}}
	records := []session.Record{
		{Seq: 1, TransactionID: "session-created-legacy", Event: &session.SessionCreated{SessionID: id}},
		{Seq: 2, TransactionID: "session-created-legacy", Event: metadata},
	}
	check := func(outbound, local bool) {
		t.Helper()
		snap, err := archive.FromRecords(records, id)
		if err != nil {
			t.Fatal(err)
		}
		resumed, err := ResolveResumeConfig(&cfg, snap)
		if err != nil || resumed.NetworkAccess != outbound || resumed.LocalDevelopmentNetwork != local {
			t.Fatalf("restored outbound=%v local=%v err=%v", resumed.NetworkAccess, resumed.LocalDevelopmentNetwork, err)
		}
	}
	cfg.NetworkAccess, cfg.LocalDevelopmentNetwork = true, false
	check(true, false)
	cfg.NetworkAccess, cfg.LocalDevelopmentNetwork = false, true
	check(false, true)
	// An explicit later restriction must still win over the current defaults.
	cfg.NetworkAccess, cfg.LocalDevelopmentNetwork = true, true
	records = append(records, session.Record{Seq: 3, TransactionID: "change-policy", Event: &session.SettingsChanged{
		Revision: 2, Settings: session.Settings{Workspace: cfg.Workspace, PermissionPolicy: "auto"},
	}})
	check(false, false)
}

func TestWorkerInheritsNetworkBaseline(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, workspace := range []string{"shared", "isolated"} {
			t.Run(map[bool]string{true: "default", false: "disabled"}[enabled]+"/"+workspace, func(t *testing.T) {
				cfg := isolatedTestConfig(t, "network-inheritance")
				if enabled && !cfg.NetworkAccess {
					t.Fatal("default configuration disabled network")
				}
				cfg.NetworkAccess = enabled
				cfg.NetworkProxy = "http://127.0.0.1:7897"
				cfg.PermissionMode = string(permissions.ModeManual)
				provider := &policyTestProvider{name: cfg.Model}
				models := plugin.NewModelRegistry()
				models.Register(provider)
				models.Route(cfg.Model, provider.Name())
				parent, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = parent.CloseContext(context.Background()) })
				childCfg, opts, err := childOptionsFromParent(parent, childPurposeTask)
				if err != nil {
					t.Fatal(err)
				}
				root := cfg.Workspace
				if workspace == "isolated" {
					root = t.TempDir()
				}
				bindChildWorkspace(&childCfg, &opts, workspace, root, cfg.Workspace)
				applyChildRole(&childCfg, &opts, "worker", workspace)
				child, err := testNewWithOptions(t, &childCfg, opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = child.CloseContext(context.Background()) })
				if child.currentSandbox().Snapshot().NetworkProxy != cfg.NetworkProxy {
					t.Fatal("child lost trusted network proxy")
				}
				call := messages.ToolCall{Name: "WebFetch"}
				tool := capabilityTestTool(t, child, call.Name)
				if denied, reason := ChildToolGate(child, call, tool); denied {
					t.Fatalf("WebFetch unexpectedly requires tool approval: %s", reason)
				}
				policy, requested, _, err := child.toolCapabilityPolicy(call, tool)
				if enabled {
					if err != nil || len(requested) != 0 || !policy.NetworkAllowed() {
						t.Fatalf("WebFetch did not inherit network: requests=%v err=%v", requested, err)
					}
				} else if err == nil && (len(requested) == 0 || policy.NetworkAllowed()) {
					t.Fatal("WebFetch bypassed disabled network")
				}
			})
		}
	}
}

func TestChildRoleAndWorkspacePreserveSessionNetworkGrant(t *testing.T) {
	for _, role := range []string{"worker", "explorer"} {
		for _, workspace := range []string{"shared", "isolated"} {
			for _, scope := range []string{"session", "once"} {
				t.Run(role+"/"+workspace+"/"+scope, func(t *testing.T) {
					parent := capabilityTestAgent(t)
					call := messages.ToolCall{Name: "WebFetch"}
					policy, requested, version, err := parent.toolCapabilityPolicy(call, capabilityTestTool(t, parent, call.Name))
					if err != nil || len(requested) != 1 {
						t.Fatalf("network request: %v %v", requested, err)
					}
					if scope == "session" {
						_, _, err = parent.addSessionCapabilities(version, requested)
					} else {
						err = applyCapability(policy, requested[0])
					}
					if err != nil {
						t.Fatal(err)
					}
					cfg, opts, err := childOptionsFromParent(parent, childPurposeTask)
					if err != nil {
						t.Fatal(err)
					}
					root := cfg.Workspace
					if workspace == "isolated" {
						root = t.TempDir()
					}
					bindChildWorkspace(&cfg, &opts, workspace, root, cfg.Workspace)
					applyChildRole(&cfg, &opts, role, workspace)
					child, err := testNewWithOptions(t, &cfg, opts)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(child.Close)
					if denied, why := ChildToolGate(child, call, capabilityTestTool(t, child, call.Name)); denied {
						t.Fatalf("role denied WebFetch: %s", why)
					}
					policy, requested, _, err = child.toolCapabilityPolicy(call, capabilityTestTool(t, child, call.Name))
					if scope == "session" {
						if err != nil || len(requested) != 0 || !policy.NetworkAllowed() {
							t.Fatalf("lost inherited network grant: %v %v", requested, err)
						}
					} else if err == nil && (len(requested) == 0 || policy.NetworkAllowed()) {
						t.Fatal("one-shot parent grant leaked to non-interactive child")
					}
				})
			}
		}
	}
}

func TestInteractiveChildCanRequestMissingNetworkCapability(t *testing.T) {
	parent := capabilityTestAgent(t)
	parent.SetChildInteraction(true)
	cfg, opts, lease, _, _, err := parent.services.supervisor.prepare(parent, childPurposeTask, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	child, err := testNewWithOptions(t, &cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(child.Close)
	_, requested, _, err := child.toolCapabilityPolicy(messages.ToolCall{Name: "WebFetch"}, tools.NewWebFetchTool())
	if err != nil || len(requested) != 1 || requested[0] != (protocol.CapabilityRequest{Kind: "network", Access: "outbound"}) {
		t.Fatalf("child blocked before reaching approval channel: %v %v", requested, err)
	}
}
