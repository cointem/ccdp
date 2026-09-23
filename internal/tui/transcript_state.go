package tui

import (
	"ccdp/internal/protocol"
)

// transcriptState owns the state of its component within a session.
type transcriptState struct {
	CellStore
	// inline owns only terminal presentation state. Runtime history remains in
	// confirmedItems/reports; the ordinary-screen renderer prints completed
	// units through tea.Println and keeps active output bounded in the frame.
	inline     historyLedger
	inlineMode bool
	// nativeHistoryResetPending is armed when the runtime replaced the
	// conversation projection (for example /clear). Rows already handed to the
	// terminal's native scrollback belong to the discarded conversation, so the
	// next update must erase that scrollback before rendering the new baseline.
	nativeHistoryResetPending bool
	transcriptScroll          ScrollState
	streaming                 bool // an assistant message is being streamed
	turnDone                  bool // the latest turn reached a terminal event/snapshot
	turnFailed                bool // latest turn ended with an error/cancellation
	interruptRequested        bool
	busy                      bool
	activity                  Activity
	pendingInputs             []protocol.InputView
}
