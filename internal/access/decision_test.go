package access

import (
	"ccdp/internal/protocol"
	"testing"
)

func TestResourceIdentityIgnoresOrderButNotAuthority(t *testing.T) {
	a := protocol.CapabilityRequest{Kind: "directory", Path: "/one", Access: "write"}
	b := protocol.CapabilityRequest{Kind: "network", Access: "outbound"}
	key := Key("owner", 1, []protocol.CapabilityRequest{a, b}, "")
	if key != Key("owner", 1, []protocol.CapabilityRequest{b, a}, "") {
		t.Fatal("request order split one resource decision")
	}
	for _, other := range []string{Key("other", 1, []protocol.CapabilityRequest{a, b}, ""), Key("owner", 2, []protocol.CapabilityRequest{a, b}, ""), Key("owner", 1, []protocol.CapabilityRequest{a, b}, "manual-command")} {
		if key == other {
			t.Fatal("decision crossed owner, epoch or operation")
		}
	}
}
