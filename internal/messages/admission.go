package messages

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var messageIDCounter uint64

func EnsureID(message *Message) {
	if message == nil || message.ID != "" {
		return
	}
	// Runtime messages receive an identity once, at admission to Agent history.
	// It must not be content-derived: two identical messages are still distinct
	// conversation entries. The identity is then carried by Session.Message and
	// retained verbatim during replay.
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		message.ID = "message-" + hex.EncodeToString(random[:])
		return
	}
	sequence := atomic.AddUint64(&messageIDCounter, 1)
	message.ID = fmt.Sprintf("message-%d-%d", time.Now().UnixNano(), sequence)
}
