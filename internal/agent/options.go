package agent

import (
	"context"

	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

// Options is the frozen input passed to the provider-owned child constructor.
// The config must already include all parent effective/project settings;
// child construction must not load settings again. ProviderRegistry and
// Permissions are snapshots/copies, never mutable parent handles. An
// AllowedTools nil map means the frozen registry may be used as-is, while a
// non-nil map (including empty) is an exact hard allowlist.
type Options struct {
	journalBackend        journal.BackendOptions
	goalOwner             goalAccountTarget
	goalOwnerID           string
	Supervisor            *SessionSupervisor
	RootContext           context.Context
	EffectiveConfigFrozen bool
	ProviderRegistry      *plugin.ModelRegistry
	Permissions           *permissions.Manager
	NonInteractive        bool
	ReadOnlyWorkspace     bool
	AllowedTools          map[string]bool
	Purpose               string
	ParentSessionID       string
	InheritedCapabilities []protocol.CapabilityRequest
	ExecutionGuard        *sandbox.ExecutionGuard
	memoryResume          []session.Record
	memoryBlobs           journal.ArtifactAccess
}
