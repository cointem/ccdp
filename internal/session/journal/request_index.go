package journal

import (
	"errors"
	"fmt"

	"ccdp/internal/session"
)

// RequestIndex retains settlement identities, never request bodies. The writer
// serializes access; this value does not introduce another coordination lock.
type RequestIndex struct {
	manifests map[string]session.RequestManifest
	admitted  map[string]bool
	finished  map[string]session.Attempt
	shared    map[string]bool
}

// Apply indexes a committed fact. live distinguishes this writer's admissions
// from unfinished requests recovered from a previous process.
func (x *RequestIndex) Apply(event session.Event, live bool) {
	switch e := session.EventPointer(event).(type) {
	case *session.RequestPrepared:
		m := e.Manifest
		m.Messages = nil
		m.MessageRefs = nil
		m.WireBody = nil
		m.Tools = nil
		if m.ReservedUsage != nil {
			v := *m.ReservedUsage
			m.ReservedUsage = &v
		}
		if x.manifests == nil {
			x.manifests = make(map[string]session.RequestManifest)
		}
		x.manifests[m.RequestID] = m
		if live {
			if x.admitted == nil {
				x.admitted = make(map[string]bool)
			}
			x.admitted[m.RequestID] = true
		}
	case *session.AttemptFinished:
		if x.finished == nil {
			x.finished = make(map[string]session.Attempt)
		}
		x.finished[e.Attempt.RequestID] = e.Attempt
	case *session.UsageChanged:
		if e.SourceSessionID != "" && e.SourceAttemptID != "" {
			if x.shared == nil {
				x.shared = make(map[string]bool)
			}
			x.shared[e.SourceSessionID+"/"+e.SourceAttemptID] = true
		}
	}
}

func (x *RequestIndex) Manifest(id string) (session.RequestManifest, bool) {
	m, ok := x.manifests[id]
	if m.ReservedUsage != nil {
		v := *m.ReservedUsage
		m.ReservedUsage = &v
	}
	return m, ok
}

func (x *RequestIndex) Admitted(id string) bool { return x.admitted[id] }

func (x *RequestIndex) Finished(id string) bool { _, ok := x.finished[id]; return ok }

// CompletedAttempt returns the value used for idempotent settlement. Attempts
// contain only value fields; no request body or mutable projection escapes.
func (p *Writer) CompletedAttempt(requestID string) (session.Attempt, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	attempt, ok := p.requests.finished[requestID]
	return attempt, ok
}

func (x *RequestIndex) SharedUsageAccounted(sessionID, attemptID string) bool {
	return x.shared[sessionID+"/"+attemptID]
}

var ErrRequestNotFound = errors.New("agent: prepared request not found")

func (p *Writer) SharedUsageAccounted(sessionID, attemptID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests.SharedUsageAccounted(sessionID, attemptID)
}

func (p *Writer) CommittedRequestManifest(requestID string) (session.RequestManifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.RequestManifest{}, session.ErrClosed
	}
	manifest, ok := p.requests.Manifest(requestID)
	if !ok {
		return session.RequestManifest{}, fmt.Errorf("%w: %s", ErrRequestNotFound, requestID)
	}
	if manifest.ReservedUsage != nil {
		usage := *manifest.ReservedUsage
		manifest.ReservedUsage = &usage
	}
	return manifest, nil
}

// This flag survives completion for the lifetime of the writer. A recovery
// snapshot may precede a concurrent completion; it must never settle that
// still-live lifecycle as a crashed attempt.
func (p *Writer) RequestAdmitted(requestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests.Admitted(requestID)
}

func (p *Writer) RequestFinished(requestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests.Finished(requestID)
}

const RequestWireFormat = "llm.completion_request.v1"
