package desktop

import (
	"slices"
	"strings"

	"ccdp/internal/protocol"
)

// projection consumes the protocol's identified cumulative transcript. Raw
// token deltas and History are deliberately not a second conversation source.
type projection struct {
	view       protocol.SessionView
	children   []protocol.ChildSession
	notice     string
	noticeTurn protocol.TurnID
}

func (p *projection) apply(update protocol.Update) {
	var checkErrors []protocol.TranscriptItem
	if update.Snapshot != nil {
		p.view = *update.Snapshot
		checkErrors = p.view.Transcript
	}
	if update.State != nil {
		p.view.ApplyState(*update.State)
		checkErrors = p.view.Transcript
	}
	if update.Catalog != nil {
		p.view.Catalog = *update.Catalog
	}
	if update.Child != nil {
		child := update.Child.Session
		i := slices.IndexFunc(p.children, func(c protocol.ChildSession) bool { return c.SessionID == child.SessionID })
		if i < 0 {
			p.children = append(p.children, child)
		} else {
			p.children[i] = child
		}
	}
	if event := update.Event; event != nil && (event.SessionID == "" || event.SessionID == p.view.SessionID) {
		if event.Transcript != nil && event.Transcript.ID != "" {
			p.upsert(*event.Transcript)
			if event.Transcript.Kind == "error" {
				checkErrors = []protocol.TranscriptItem{*event.Transcript}
			}
		} else if event.Kind == protocol.EventUserMessage && event.MessageID != "" {
			p.upsert(protocol.TranscriptItem{ID: event.MessageID, Kind: "user", Text: event.Text, TurnID: event.TurnID, Status: "completed"})
		}
		if event.Kind == protocol.EventError {
			p.noticeTurn = event.TurnID
			p.notice = event.Error
			if p.notice == "" {
				p.notice = event.Text
			}
			checkErrors = p.view.Transcript
		}
	}
	if update.Receipt != nil && update.Receipt.Rejected() && update.Receipt.Error != nil {
		p.notice = update.Receipt.Error.Message
		p.noticeTurn = ""
	}
	if p.noticeTurn != "" && p.notice != "" {
		for _, item := range checkErrors {
			if item.Kind == "error" && sameTurn(item.TurnID, p.noticeTurn) && item.Text == p.notice {
				p.notice, p.noticeTurn = "", ""
				break
			}
		}
	}
}

func (p *projection) upsert(item protocol.TranscriptItem) {
	items := p.view.Transcript
	if i := slices.IndexFunc(items, func(v protocol.TranscriptItem) bool { return v.ID == item.ID }); i >= 0 {
		items[i] = item
		if item.PreviousID != "" && item.PreviousID != item.ID {
			p.view.Transcript = slices.DeleteFunc(items, func(v protocol.TranscriptItem) bool { return v.ID == item.PreviousID })
		}
		return
	}
	if item.PreviousID != "" {
		if i := slices.IndexFunc(items, func(v protocol.TranscriptItem) bool { return v.ID == item.PreviousID }); i >= 0 {
			// PreviousID names the live identity replaced by a durable commit,
			// not a preceding message in display order.
			items[i] = item
			return
		}
	}
	p.view.Transcript = append(items, item)
}

func sessionTitle(view protocol.SessionView) string {
	for _, item := range view.Transcript {
		if item.Kind == "user" && strings.TrimSpace(item.Text) != "" {
			return shorten(strings.Join(strings.Fields(item.Text), " "), 40)
		}
	}
	return "新会话"
}

func shorten(text string, length int) string {
	runes := []rune(text)
	if len(runes) > length {
		return string(runes[:length]) + "…"
	}
	return text
}
