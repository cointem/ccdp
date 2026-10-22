package tui

import (
	"ccdp/internal/protocol"
	"context"
	tea "github.com/charmbracelet/bubbletea"
	"strconv"
	"strings"
	"time"
)

type agentHistoryMsg struct {
	sessionID  string
	page       protocol.TranscriptPage
	err        error
	request    uint64
	generation uint64
}
type agentOutputMsg struct {
	sessionID, itemID string
	offset            int64
	page              protocol.OutputPage
	err               error
	request           uint64
	generation        uint64
}

func (m *Model) loadAgentHistory(before uint64) tea.Cmd {
	if m.routing == nil {
		return nil
	}
	id, directory, generation := m.sessionID, m.routing.directory, m.attachmentGeneration
	m.readerSeq++
	request := m.readerSeq
	m.readerRequest = request
	if m.reader != nil {
		m.reader.loading = true
		m.reader.request = request
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		page, err := directory.ReadTranscript(ctx, protocol.SessionID(id), before, 64)
		return agentHistoryMsg{sessionID: id, page: page, err: err, request: request, generation: generation}
	}
}
func (m *Model) loadAgentOutput(id string, offset int64) tea.Cmd {
	m.readerSeq++
	m.readerRequest = m.readerSeq
	return m.loadAgentOutputWithRequest(id, offset, m.readerRequest)
}
func (m *Model) loadAgentOutputWithRequest(id string, offset int64, request uint64) tea.Cmd {
	if m.routing == nil {
		return nil
	}
	sid, directory, generation := m.sessionID, m.routing.directory, m.attachmentGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		page, err := directory.ReadOutput(ctx, protocol.SessionID(sid), id, offset, readerPageSize)
		return agentOutputMsg{sessionID: sid, itemID: id, offset: offset, page: page, err: err, request: request, generation: generation}
	}
}
func (m *Model) applyAgentHistory(msg agentHistoryMsg) bool {
	if msg.generation != m.attachmentGeneration || msg.sessionID != m.sessionID || msg.request != 0 && msg.request != m.readerRequest {
		return false
	}
	if msg.err != nil {
		m.pushStatus("history: " + msg.err.Error())
		if m.reader != nil {
			m.reader.loading = false
			m.reader.errText = "history: " + msg.err.Error()
		}
		return false
	}
	entries := readerEntriesFromTranscript(msg.page.Items)
	if len(entries) == 0 {
		m.pushStatus("no saved history")
		if m.reader != nil {
			m.reader.loading = false
			m.reader.historyMore = false
		}
		return false
	}
	previous := m.reader
	r := &readerState{workspace: m.workspace, kind: readerTranscript, title: "Saved history", sessionID: protocol.SessionID(msg.sessionID), entries: entries, index: len(entries) - 1, request: msg.request, returnOffset: m.viewport.YOffset, returnFollow: m.followOutput, historyBefore: msg.page.Before, historyMore: msg.page.More}
	if previous != nil {
		r.screenEntered = previous.screenEntered
		r.returnOffset = previous.returnOffset
		r.returnFollow = previous.returnFollow
		// Retain only one newer page as a return target; history paging remains bounded.
		previous.returnReader = nil
		r.returnReader = previous
	}
	m.reader = r
	return true
}
func (m *Model) agentOutputSelection(id string) tea.Cmd {
	if strings.HasPrefix(id, "history-before:") {
		before, _ := strconv.ParseUint(strings.TrimPrefix(id, "history-before:"), 10, 64)
		return m.loadAgentHistory(before)
	}
	return m.loadAgentOutput(id, 0)
}
func (m *Model) applyAgentOutput(msg agentOutputMsg) bool {
	if msg.generation != m.attachmentGeneration || msg.sessionID != m.sessionID || msg.request != 0 && msg.request != m.readerRequest {
		return false
	}
	if msg.err != nil {
		m.pushStatus("output: " + msg.err.Error())
		if m.reader != nil {
			m.reader.loading = false
			m.reader.errText = "output: " + msg.err.Error()
		}
		return false
	}
	r := m.reader
	if r != nil && r.kind == readerOutput && r.itemID == msg.itemID && r.request == msg.request {
		if msg.offset > r.pageOffset {
			r.previousOffsets = append(r.previousOffsets, r.pageOffset)
		} else if n := len(r.previousOffsets); n > 0 && msg.offset == r.previousOffsets[n-1] {
			r.previousOffsets = r.previousOffsets[:n-1]
		}
		r.text, r.rawText = msg.page.Text, msg.page.Text
		r.pageOffset = msg.offset
		r.next = msg.page.Next
		r.total = msg.page.Total
		r.more = msg.page.More
		r.loading = false
		r.errText = ""
		r.offset = 0
		r.cacheWidth = 0
		r.matches = nil
		r.locations = nil
		r.rebuildLines(max(1, r.width))
		if r.search != "" {
			r.matches = r.findMatches()
			r.selectMatch(0)
		}
		return false
	}
	m.setOutputReader(msg)
	return true
}
