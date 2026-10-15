package agent

import (
	"context"
	"testing"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

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
				cfg.PermissionMode = string(permissions.ModeDefault)
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
				for _, name := range []string{"WebSearch", "WebFetch"} {
					call := messages.ToolCall{Name: name}
					if denied, reason := ChildToolGate(child, call, capabilityTestTool(t, child, call.Name)); denied {
						t.Fatalf("%s unexpectedly requires tool approval: %s", name, reason)
					}
					policy, requested, _, err := child.toolCapabilityPolicy(call, capabilityTestTool(t, child, call.Name))
					if enabled {
						if err != nil || len(requested) != 0 || !policy.NetworkAllowed() {
							t.Fatalf("%s did not inherit network: requests=%v err=%v", name, requested, err)
						}
					} else if err == nil {
						t.Fatalf("%s bypassed disabled network", name)
					}
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
					call := messages.ToolCall{Name: "WebSearch"}
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
						t.Fatalf("role denied WebSearch: %s", why)
					}
					policy, requested, _, err = child.toolCapabilityPolicy(call, capabilityTestTool(t, child, call.Name))
					if scope == "session" {
						if err != nil || len(requested) != 0 || !policy.NetworkAllowed() {
							t.Fatalf("lost inherited network grant: %v %v", requested, err)
						}
					} else if err == nil {
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
