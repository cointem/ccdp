package agent

import (
	"context"
	"fmt"
	"math"

	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

const compactOutputPreviewBytes = 512

// pruneToolOutputs changes only old successful tool-result text. Calls,
// provider-native content, errors and recent complete groups remain untouched.
func (a *Agent) pruneToolOutputs(ctx context.Context, history []messages.Message, keep int) ([]messages.Message, bool) {
	candidate := messages.Clone(history)
	cut := compactCut(history, keep, math.MaxInt)
	var blobs journal.ArtifactReader
	verified := make(map[string]bool)
	changed := false
	for i := 0; i < cut; i++ {
		message := &candidate[i]
		if message.Role != messages.RoleTool || (message.ToolStatus != "success" && message.ToolStatus != "ok") ||
			message.OutputRef == nil || len(message.ProviderContent) != 0 || len(message.Content) <= 2*compactOutputPreviewBytes {
			continue
		}
		if ctx.Err() != nil {
			return candidate, changed
		}
		ref := session.BlobRef{Hash: message.OutputRef.Hash, Size: message.OutputRef.Size, MediaType: message.OutputRef.MediaType}
		if journal.ValidateBlobRef(ref) != nil {
			continue
		}
		if blobs == nil {
			var err error
			blobs, err = a.historyArtifacts()
			if err != nil {
				return candidate, changed
			}
		}
		if !verified[ref.Hash] {
			if _, err := blobs.Read(ref, session.DefaultMaxBlobBytes); err != nil {
				// A missing/corrupt artifact must never authorize dropping text.
				continue
			}
			verified[ref.Hash] = true
		}
		message.Content = messages.TruncateUTF8(message.Content, compactOutputPreviewBytes) +
			fmt.Sprintf("\n[Earlier successful tool output shortened. Original: %d bytes. ReadOutput output_id=%q offset=0 limit=16384]", ref.Size, ref.Hash)
		message.TokenCount = 0
		message.DisplayTruncated = true
		changed = true
	}
	return candidate, changed
}

func collectOutputReferences(history []messages.Message) []messages.OutputReference {
	var refs []messages.OutputReference
	seen := make(map[string]bool)
	add := func(ref messages.OutputReference) {
		if ref.Hash != "" && !seen[ref.Hash] {
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	for _, message := range history {
		if message.OutputRef != nil {
			add(*message.OutputRef)
		}
		for _, ref := range message.SourceOutputs {
			add(ref)
		}
	}
	return refs
}
