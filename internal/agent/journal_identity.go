package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func inputMessageID(inputID string) string {
	return protocol.InputMessageID(protocol.InputID(inputID))
}

func snapshotMessageID(sessionID string, index int) string {
	return messages.StableID("legacy-message", struct {
		SessionID string
		Index     int
	}{SessionID: sessionID, Index: index})
}
