package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/session"
)

// The owner and journal share this one raw replay.State. Recovery-only tool
// results are a read view until the next explicit conversation commit; they
// never mutate raw tool facts or authorize replaying a side effect.
func (a *Agent) historyLocked() []messages.Message {
	if a.conversation.recoveryHistory != nil && a.conversation.recoveryBase <= len(a.conversation.durable.History) {
		length := len(a.conversation.durable.History)
		if a.conversation.recoveryView == nil || a.conversation.recoveryLength != length {
			a.conversation.recoveryView = append(a.conversation.recoveryHistory[:len(a.conversation.recoveryHistory):len(a.conversation.recoveryHistory)], a.conversation.durable.History[a.conversation.recoveryBase:]...)
			a.conversation.recoveryLength = length
		}
		return a.conversation.recoveryView
	}
	a.conversation.recoveryView = nil
	return a.conversation.durable.History
}

func sessionUsage(u Usage) session.Usage {
	return session.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CachedTokens: int64(u.CachedTokens), TurnCount: int64(u.TurnCount), TotalTokens: int64(u.InputTokens) + int64(u.OutputTokens), Cache: u.Cache}
}

func (a *Agent) runtimeUsageLocked() Usage {
	u, err := usageFromSession(a.conversation.durable.Usage)
	if err != nil {
		panic(err)
	} // constructor and checked accounting reject unrepresentable counters
	return u
}
