package agent

import (
	"errors"
	"fmt"

	"ccdp/internal/llm"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

func (p *sessionPersistence) loadPreparedRequest(requestID string) (archive.PreparedRequest, error) {
	if p == nil {
		return archive.PreparedRequest{}, errors.New("agent: nil session persistence")
	}
	records, blobs, err := p.ReadSnapshot()
	if err != nil {
		return archive.PreparedRequest{}, err
	}
	prepared, err := archive.PreparedFromRecords(records, blobs, requestID)

	if err == nil && prepared.Manifest.SessionID != "" && p.Identity() != "" && prepared.Manifest.SessionID != p.Identity() {
		return archive.PreparedRequest{}, fmt.Errorf("%w: prepared request session mismatch", ErrPreparedRequestBlobCorrupt)
	}
	return prepared, err
}

// LoadPreparedRequest loads and verifies a frozen request from a JSONL
// session directory. Legacy sessions without RequestPrepared/wire facts are
// reported unavailable rather than being given a fabricated request.
func LoadPreparedRequest(sessionRoot, sessionID, requestID string) (llm.CompletionRequest, error) {
	store, err := session.OpenJSONLReadOnly(sessionRoot, sessionID)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	defer store.Close()
	blobs := session.OpenArtifactStoreReadOnly(store.Dir())
	prepared, err := archive.ReadPreparedRequest(store, blobs, requestID)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	if prepared.Manifest.SessionID != "" && prepared.Manifest.SessionID != sessionID {
		return llm.CompletionRequest{}, fmt.Errorf("%w: prepared request session mismatch", ErrPreparedRequestBlobCorrupt)
	}
	return prepared.Request, nil
}

func (a *Agent) LoadPreparedRequest(requestID string) (llm.CompletionRequest, error) {
	if a == nil {
		return llm.CompletionRequest{}, errors.New("agent: nil agent")
	}
	p := a.persistenceHandle()
	if p == nil {
		return llm.CompletionRequest{}, errors.New("agent: session persistence is unavailable")
	}
	prepared, err := p.loadPreparedRequest(requestID)
	if err != nil {
		return llm.CompletionRequest{}, err
	}
	return prepared.Request, nil
}
