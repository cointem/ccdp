package tui

import (
	"ccdp/internal/protocol"
	"github.com/charmbracelet/bubbles/textarea"
)

// composerState owns the state of its component within a session.
type composerState struct {
	textarea             textarea.Model
	clipboard            Clipboard
	clipboardReader      ClipboardReader
	pasteRequest         protocol.CommandID
	inputImages          []protocol.InputImage
	selectedImage        int
	retryImages          []protocol.InputImage
	historyImages        map[int][]protocol.InputImage
	expiredHistoryImages map[int]bool
	missingHistoryImages bool
	history              []string
	historyIdx           int
	// cmdSug is the live slash-command autocomplete list (popup above the
	// input) while the user is typing a command name after "/". cmdSugIdx is
	// the highlighted entry; navigation covers the complete result set while
	// the popup renders a small window around the selection.
	cmdSug    []string
	cmdSugIdx int

	// histSearch is the transient reverse-search state opened by ctrl+r. It is
	// nil except while the search prompt owns the key stream.
	histSearch *historySearch

	// fileMention is the transient @-completion popup for the trailing composer
	// token. It is nil except while a mention is being completed.
	fileMention *fileMention
	// pasteFold retains the original long paste while its compact preview is
	// shown near the composer. The underlying textarea always remains editable.
	pasteFold *pasteFoldState
}
