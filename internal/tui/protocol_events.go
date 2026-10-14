package tui

import (
	"crypto/sha256"

	"encoding/json"
	"fmt"

	"strings"

	"time"

	"ccdp/internal/protocol"
)

// handleProtocolEvent updates transient presentation only. Confirmed model,
// mode, sandbox and plan state continues to come from snapshots.
func (m *Model) handleProtocolEvent(ev protocol.EventView) {
	if ev.SessionID != "" && ev.SessionID.String() != m.sessionID {
		return
	}
	if ev.Kind == protocol.EventOperationProgress {
		// Review progress uses ordinary transcript cells but does not own a
		// conversation turn or its streaming/completion lifecycle.
		if ev.Transcript != nil && ev.Transcript.ID != "" {
			m.upsertTranscript(*ev.Transcript)
		}
		return
	}
	// Project the event into the activity lane before handling transcript data.
	// The transcript path can return early for a typed stream/tool update, but
	// its phase still needs to be visible to the renderer.
	m.updateActivityFromEvent(ev)
	m.associateInputOperationFromEvent(ev)
	if ev.Transcript != nil && ev.Transcript.ID != "" {
		switch ev.Kind {
		case protocol.EventStream:
			m.upsertTranscript(*ev.Transcript)
			if ev.Text == "" {
				// Reasoning-close signal: it finalizes the thinking cell, it is
				// not answer text, so it must not (re)mark the turn as streaming.
				m.streaming = false
				return
			}
			m.streaming, m.turnDone = true, false
			return
		case protocol.EventReasoning, protocol.EventToolStarted, protocol.EventToolProgress, protocol.EventToolResult:
			m.upsertTranscript(*ev.Transcript)
			m.streaming = false
			return
		}
	}
	switch ev.Kind {
	case protocol.EventGoalChanged:
		m.snapshot.Goal = ev.Goal
	case protocol.EventUserMessage:
		if !m.inline.primed {
			m.inline.prime(m.items)
		}
		if ev.MessageID != "" {
			m.upsertTranscript(protocol.TranscriptItem{ID: ev.MessageID, Kind: "user", TurnID: ev.TurnID, Text: ev.Text, Status: "completed"})
		}
		if !m.busy || m.turnStarted.IsZero() {
			m.turnStarted = now()
		}
		m.busy, m.streaming, m.turnDone = true, false, false
		m.turnFailed, m.interruptRequested = false, false
	case protocol.EventStream, protocol.EventReasoning, protocol.EventToolStarted, protocol.EventToolProgress:
		// Transcript events must carry an identified cumulative projection.
		// Missing projections never create a second, id-less transcript.
		return
	case protocol.EventToolResult:
		operationReport := false
		if ev.Tool != nil {
			_, operationReport = m.operationReports[protocol.CommandID(ev.Tool.ID.String())]
		}
		if ev.Tool != nil && (strings.HasPrefix(ev.Tool.Name, "query:") || operationReport || ev.Tool.Name == "review" || ev.Tool.Name == "restore" || ev.Tool.Name == "delivery" || ev.Tool.Name == "merge child") {
			// Typed operation/query commands use the generic operation event bridge.
			// Promote their bounded output to a permanent report rather than a
			// transient tool row that a later snapshot would erase.
			kind := "system"
			if ev.Tool.Status == "error" || ev.Tool.Status == "denied" {
				kind = "error"
			}
			text := codingReport(strings.TrimSpace(ev.Tool.Output))
			if text == "" {
				text = "(empty query result)"
			}
			m.addReport(historyCell{kind: kind, text: text, reportID: ev.Tool.ID.String()})
			delete(m.operationReports, protocol.CommandID(ev.Tool.ID.String()))
		}
	case protocol.EventStatus:
		if strings.TrimSpace(ev.Text) != "" {
			m.addNotice(Notice{Severity: NoticeInfo, Text: ev.Text, Sticky: false})
		}
	case protocol.EventError:
		text := ev.Error
		if text == "" {
			text = ev.Text
		}
		// The transcript owns the error explanation. A sticky notice with the
		// same body would repeat it indefinitely above the composer.
		if !m.hasTurnError(ev.TurnID, text) {
			id := ""
			if ev.TurnID != "" {
				id = fmt.Sprintf("error-event:%s:%x", canonicalUITurn(ev.TurnID), sha256.Sum256([]byte(text)))
			}
			m.addReport(historyCell{kind: "error", text: text, turnID: ev.TurnID, reportID: id})
		}
		m.turnFailed = true
	case protocol.EventApprovalRequest:
		if ev.Approval != nil {
			m.clearDecisionNotices()
			m.approval = approvalRequestFromView(ev.Approval)
			m.approvalState.Offset = 0
			m.addNotice(Notice{Severity: NoticeDecision, Text: "awaiting approval…", Sticky: true})
			m.notifyUser("ccdp needs your decision", ev.Approval.Tool)
		}
	case protocol.EventQuestionRequest:
		m.setQuestion(ev.Question)
		if ev.Question != nil {
			m.notifyUser("ccdp has a question", "waiting for your answer")
		}
	case protocol.EventPlanReady:
		if ev.Plan != nil {
			m.clearDecisionNotices()
			m.addReport(historyCell{kind: "system", text: "── plan proposed ──"})
			m.addReport(historyCell{kind: "assistant", text: ev.Plan.Text})
			m.approval = &approvalPrompt{ID: ev.Plan.ID, Tool: "Plan", Command: ev.Plan.Text, Reason: "Approve this plan and start executing?"}
			m.approvalState.Offset = 0
			m.addNotice(Notice{Severity: NoticeDecision, Text: "awaiting plan approval…", Sticky: true})
			m.notifyUser("ccdp needs your decision", "a plan is ready to approve")
		}
	case protocol.EventUsage:
		if ev.Usage != nil {
			m.usage = protocol.UsageSnapshot{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens, CachedTokens: ev.Usage.CachedTokens, Cache: ev.Usage.Cache, TurnCount: ev.Usage.TurnCount}
		}
	case protocol.EventTurnDone:
		outcome := protocol.TurnSucceeded
		if m.turnFailed {
			outcome = protocol.TurnFailed
		} else if m.interruptRequested {
			outcome = protocol.TurnCancelled
		}
		m.finishInputOperations(ev.TurnID, outcome, ev.Error)
		m.busy, m.streaming, m.turnDone = false, false, true
		m.clearTransientNotices()
		m.finishActivity("", ActivityCompleted, "")
		m.turnFailed = m.turnFailed || m.interruptRequested
		if goal := m.snapshot.Goal; goal != nil {
			if goal.Status != protocol.GoalActive {
				m.notifyUser("ccdp goal "+string(goal.Status), goal.Reason)
			}
		} else if !m.turnStarted.IsZero() && now().Sub(m.turnStarted) > 15_000_000_000 {
			m.notifyUser("ccdp finished", "a turn completed")
		}
	case protocol.EventStateChanged:
		// The accompanying state snapshot is the authority. Do not mutate
		// confirmed settings from an event that may be stale.
	}
}

func approvalRequestFromView(view *protocol.ApprovalView) *approvalPrompt {
	if view == nil {
		return nil
	}
	command := ""
	if len(view.Args) > 0 {
		var object map[string]any
		if json.Unmarshal(view.Args, &object) == nil {
			if raw, ok := object["command"].(string); ok {
				command = raw
			}
			if command == "" {
				command = string(view.Args)
			}
		} else {
			command = string(view.Args)
		}
	}
	return &approvalPrompt{ID: view.ID, Tool: view.Tool, Command: command, Reason: view.Reason,
		Capabilities: append([]protocol.CapabilityRequest(nil), view.Capabilities...)}
}

// Small indirections keep protocol projection testable without binding the
// event path to wall-clock or process-global stdout in tests.
var now = func() time.Time { return time.Now() }

func protocolToolArgs(t *protocol.ToolView) map[string]any {
	if t == nil || len(t.Args) == 0 {
		return nil
	}
	var args map[string]any
	_ = json.Unmarshal(t.Args, &args)
	return args
}
