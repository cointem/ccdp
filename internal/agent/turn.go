package agent

import (
	"context"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

// beginTurnLocked is shared by user input, queued input and goal continuation.
// Admission owns the busy reservation and cancellation flags before this call.
func (a *Agent) beginTurnLocked(label, text string) {
	a.turn.turnCtx, a.turn.turnCancel = context.WithCancel(a.lifecycle.rootCtx)
	if a.turn.interruptFlag {
		a.turn.turnCancel()
	}
	a.sequences.turnSeq++
	a.lifecycle.turnWG.Add(1)
	a.turn.busy = true
	a.lifecycle.settling = false
	a.lifecycle.phase = protocol.PhasePreparing
	a.turn.workStartedAt = time.Now()
	a.turn.workLabel = label
	a.lifecycle.lastTurn = &protocol.TurnOutcome{TurnID: protocol.TurnID(fmt.Sprintf("%d", a.sequences.turnSeq)), Status: protocol.TurnRunning}
	a.conversation.goalTurnCalls = 0
	a.conversation.turnGoalID = ""
	if a.conversation.durable.Goal != nil && a.conversation.durable.Goal.Status == protocol.GoalActive {
		a.conversation.turnGoalID = a.conversation.durable.Goal.ID
	}
	a.decisions.approvalCache = map[string]bool{}
	a.budget.outputBudget, a.turnMemory.guardianUses = 0, 0
	a.modelsState.usedFallback = false
	if primary := a.modelsState.primaryBinding; primary.client != nil {
		a.services.client = primary.client
		a.modelsState.activeModel = primary.model
		primary.version = a.modelsState.activeBinding.version + 1
		a.modelsState.activeBinding = primary
	}
	a.turnMemory.turnUserMsg, a.turnMemory.turnSummary = text, ""
	a.turnMemory.turnTouched = nil
}
