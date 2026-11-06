// Package access defines resource decision identities independently of tools,
// command strings and the runtime's mutable owner loop.
package access

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"ccdp/internal/protocol"
)

// Key includes the authorization epoch. A resource-only decision can be
// reused across different operations; operation approvals remain distinct.
func Key(owner string, epoch uint64, resources []protocol.CapabilityRequest, operation string) string {
	if len(resources) == 0 {
		return ""
	}
	entries := make([]string, len(resources))
	for i, resource := range resources {
		b, _ := json.Marshal(resource)
		entries[i] = string(b)
	}
	sort.Strings(entries)
	b, _ := json.Marshal(struct {
		Owner     string
		Epoch     uint64
		Resources []string
		Operation string
	}{owner, epoch, entries, operation})
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}
