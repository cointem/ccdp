package journal

import (
	"ccdp/internal/session"
)

// BackendOptions decorates owned resources at construction. It supports store
// instrumentation and fault injection without exposing a live writer's store
// or allowing its backend to be replaced during execution.
type BackendOptions struct {
	Store     func(session.Store) session.Store
	Artifacts func(ArtifactAccess) ArtifactAccess
}
