package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"ccdp/internal/llm"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

var ErrPreparedRequestNotFound = journal.ErrRequestNotFound
var ErrPreparedRequestNotRebuildable = errors.New("agent: prepared request is not rebuildable")

// PreparedRequest is the verified representation used by diagnostic loaders.
// Live request settlement reads only the committed metadata index.
type PreparedRequest struct {
	Manifest session.RequestManifest
	Request  llm.CompletionRequest
	Wire     []byte
}

func ReadPreparedRequest(store session.ReadOnlyStore, blobs journal.ArtifactReader, requestID string) (PreparedRequest, error) {
	if store == nil {
		return PreparedRequest{}, errors.New("agent: prepared request store is unavailable")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return PreparedRequest{}, errors.New("agent: prepared request ID is required")
	}
	records, err := session.ReadAll(store)
	if err != nil {
		return PreparedRequest{}, err
	}
	return PreparedFromRecords(records, blobs, requestID)
}

// PreparedFromRecords verifies a detached log snapshot and read-only blob source.
func PreparedFromRecords(records []session.Record, blobs journal.ArtifactReader, requestID string) (PreparedRequest, error) {
	var manifest *session.RequestManifest
	for _, record := range records {
		var candidate *session.RequestManifest
		switch event := record.Event.(type) {
		case *session.RequestPrepared:
			candidate = &event.Manifest
		case session.RequestPrepared:
			copyManifest := event.Manifest
			candidate = &copyManifest
		}
		if candidate == nil || candidate.RequestID != requestID {
			continue
		}
		if manifest != nil {
			return PreparedRequest{}, fmt.Errorf("%w: duplicate RequestPrepared %q", journal.ErrBlobCorrupt, requestID)
		}
		copyManifest := *candidate
		manifest = &copyManifest
	}
	if manifest == nil {
		return PreparedRequest{}, fmt.Errorf("%w: %s", ErrPreparedRequestNotFound, requestID)
	}
	if manifest.WireBody == nil {
		return PreparedRequest{}, fmt.Errorf("%w: request %s has no supported wire body", ErrPreparedRequestNotRebuildable, requestID)
	}
	if manifest.WireFormat != journal.RequestWireFormat {
		return PreparedRequest{}, fmt.Errorf("%w: request %s has unsupported wire format", ErrPreparedRequestNotRebuildable, requestID)
	}
	if blobs == nil {
		return PreparedRequest{}, fmt.Errorf("%w: %s", journal.ErrBlobMissing, manifest.WireBody.Hash)
	}
	wire, err := blobs.Read(*manifest.WireBody, session.DefaultMaxBlobBytes)
	if err != nil {
		return PreparedRequest{}, ClassifyRequestBlobError(err)
	}
	sum := sha256.Sum256(wire)
	digest := hex.EncodeToString(sum[:])
	if !strings.EqualFold(digest, manifest.WireBody.Hash) || int64(len(wire)) != manifest.WireBody.Size ||
		!strings.EqualFold(digest, manifest.Digest) {
		return PreparedRequest{}, fmt.Errorf("%w: request %s digest mismatch", journal.ErrBlobCorrupt, requestID)
	}
	var req llm.CompletionRequest
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		return PreparedRequest{}, fmt.Errorf("%w: request %s JSON: %v", journal.ErrBlobCorrupt, requestID, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return PreparedRequest{}, fmt.Errorf("%w: request %s has trailing JSON", journal.ErrBlobCorrupt, requestID)
		}
		return PreparedRequest{}, fmt.Errorf("%w: request %s trailing JSON: %v", journal.ErrBlobCorrupt, requestID, err)
	}
	if !req.Stream || req.Model == "" || req.Model != manifest.Model {
		return PreparedRequest{}, fmt.Errorf("%w: request %s body identity mismatch", journal.ErrBlobCorrupt, requestID)
	}
	return PreparedRequest{Manifest: *manifest, Request: req, Wire: append([]byte(nil), wire...)}, nil
}

func ClassifyRequestBlobError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, journal.ErrBlobMissing) || errors.Is(err, journal.ErrBlobCorrupt) || errors.Is(err, session.ErrTooLarge) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", journal.ErrBlobMissing, err)
	}
	return fmt.Errorf("%w: %v", journal.ErrBlobCorrupt, err)
}
