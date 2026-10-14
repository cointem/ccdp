package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"ccdp/internal/session"
)

var ErrBlobCorrupt = errors.New("agent: prepared request blob is corrupt")
var ErrBlobMissing = errors.New("agent: prepared request blob is missing")

// ArtifactReader gives archive and diagnostic readers no write capability.
type ArtifactReader interface {
	Read(session.BlobRef, int64) ([]byte, error)
}

// ArtifactAccess is scoped to the owning session's immutable blob store.
type ArtifactAccess interface {
	ArtifactReader
	PutContext(context.Context, []byte) (session.BlobRef, error)
}

// MemoryArtifacts is the blob half of --no-session-persistence. It is
// intentionally independent of ArtifactStore because that type is
// filesystem-backed by design. Hash and size are checked both on write and
// read so memory mode has the same reconstruction contract as disk mode.
type MemoryArtifacts struct {
	mu       sync.RWMutex
	maxBytes int64
	blobs    map[string][]byte
}

func NewMemoryArtifacts(maxBytes int64) *MemoryArtifacts {
	if maxBytes <= 0 {
		maxBytes = session.DefaultMaxBlobBytes
	}
	return &MemoryArtifacts{maxBytes: maxBytes, blobs: make(map[string][]byte)}
}

func (s *MemoryArtifacts) PutContext(ctx context.Context, data []byte) (session.BlobRef, error) {
	if s == nil {
		return session.BlobRef{}, errors.New("agent: nil memory request artifacts")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return session.BlobRef{}, err
	}
	if s.maxBytes > 0 && int64(len(data)) > s.maxBytes {
		return session.BlobRef{}, fmt.Errorf("%w: request wire exceeds %d bytes", session.ErrTooLarge, s.maxBytes)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	copyData := append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.blobs[hash]; ok && !bytes.Equal(prior, copyData) {
		return session.BlobRef{}, fmt.Errorf("%w: memory blob hash collision", ErrBlobCorrupt)
	}
	s.blobs[hash] = copyData
	return session.BlobRef{Hash: hash, Size: int64(len(copyData)), MediaType: "application/json"}, nil
}

func (s *MemoryArtifacts) Read(ref session.BlobRef, maxBytes int64) ([]byte, error) {
	if s == nil {
		return nil, errors.New("agent: nil memory request artifacts")
	}
	if err := ValidateBlobRef(ref); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlobCorrupt, err)
	}
	if maxBytes <= 0 {
		maxBytes = s.maxBytes
	}
	if maxBytes > 0 && ref.Size > maxBytes {
		return nil, fmt.Errorf("%w: request wire exceeds %d bytes", session.ErrTooLarge, maxBytes)
	}
	s.mu.RLock()
	data, ok := s.blobs[ref.Hash]
	copyData := append([]byte(nil), data...)
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBlobMissing, ref.Hash)
	}
	if int64(len(copyData)) != ref.Size || (maxBytes > 0 && int64(len(copyData)) > maxBytes) {
		return nil, fmt.Errorf("%w: %s size changed", ErrBlobCorrupt, ref.Hash)
	}
	sum := sha256.Sum256(copyData)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.Hash) {
		return nil, fmt.Errorf("%w: %s hash mismatch", ErrBlobCorrupt, ref.Hash)
	}
	return copyData, nil
}

func ValidateBlobRef(ref session.BlobRef) error {
	if len(ref.Hash) != 64 {
		return errors.New("blob hash must be a SHA-256 hex string")
	}
	for _, r := range ref.Hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return errors.New("blob hash is not hexadecimal")
		}
	}
	if ref.Size < 0 {
		return errors.New("blob size must not be negative")
	}
	return nil
}
