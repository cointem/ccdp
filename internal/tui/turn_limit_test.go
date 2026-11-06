package tui

import (
	"testing"

	"ccdp/internal/protocol"
)

func TestTurnLimitsAndDenialsRemainUnsuccessfulAfterDone(t *testing.T) {
	for _, status := range []protocol.TurnOutcomeStatus{protocol.TurnLimited, protocol.TurnBlocked} {
		t.Run(string(status), func(t *testing.T) {
			m := astraModel(t, 80, 24)
			cmd := protocol.NewSubmitInput("input", protocol.SessionID(m.sessionID), "input", "work", protocol.InputSteer)
			m.registerOperation(cmd, "message")
			op := m.operations[cmd.ID]
			op.TurnID = "1"
			m.operations[cmd.ID] = op
			m.snapshot.LastTurn = &protocol.TurnOutcome{TurnID: "1", Status: status, Error: "task incomplete"}
			m.snapshot.Phase = protocol.PhaseIdle
			if activity := m.activityForSnapshot(m.snapshot); activity.Phase != ActivityFailed {
				t.Fatal(activity)
			}
			m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventTurnDone, TurnID: "1"})
			if m.operations[cmd.ID].State != OperationFailed || !m.turnFailed {
				t.Fatal("terminal event overwrote the recorded stop outcome")
			}
		})
	}
}
