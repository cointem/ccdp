package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/session"
)

func messageToSession(message messages.Message) (session.Message, error) {
	messages.EnsureID(&message)
	return session.EncodeMessage(message)
}
