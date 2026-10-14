package protocol

import "time"

// SessionState replaces all frequently-changing state fields. Unlike a full
// SessionView, it never contains the history or tool/model catalog. Nil decision
// pointers explicitly clear a decision; they never mean "field omitted".
type SessionState struct {
	HistoryLength     int                `json:"history_length"`
	OutstandingWork   int                `json:"outstanding_work"`
	Goal              *Goal              `json:"goal,omitempty"`
	Question          *QuestionRequest   `json:"question,omitempty"`
	RunID             RunID              `json:"run_id,omitempty"`
	Transcript        []TranscriptItem   `json:"transcript,omitempty"`
	TranscriptMore    bool               `json:"transcript_more,omitempty"`
	SessionID         SessionID          `json:"session_id"`
	Revision          Revision           `json:"revision"`
	Phase             RuntimePhase       `json:"phase"`
	Workflow          WorkflowState      `json:"workflow"`
	Busy              bool               `json:"busy"`
	WorkLabel         string             `json:"work_label,omitempty"`
	WorkStartedAt     time.Time          `json:"work_started_at,omitempty"`
	Closing           bool               `json:"closing"`
	Settings          SettingsSnapshot   `json:"settings"`
	SandboxRuntime    SandboxRuntimeView `json:"sandbox_runtime"`
	Usage             UsageSnapshot      `json:"usage"`
	ContextUsedTokens int                `json:"context_used_tokens"`
	SystemTokens      int                `json:"system_tokens,omitempty"`
	ToolsTokens       int                `json:"tools_tokens,omitempty"`
	MessagesTokens    int                `json:"messages_tokens,omitempty"`
	PendingInputs     []InputView        `json:"pending_inputs,omitempty"`
	Approval          *ApprovalView      `json:"approval,omitempty"`
	Plan              *PlanView          `json:"plan,omitempty"`
	Tasks             []TaskView         `json:"tasks,omitempty"`
	LastTurn          *TurnOutcome       `json:"last_turn,omitempty"`
}

// ApplyState preserves the full snapshot-only history and catalog.
func (v *SessionView) ApplyState(s SessionState) {
	v.HistoryLength = s.HistoryLength
	v.OutstandingWork = s.OutstandingWork
	v.Goal = s.Goal
	v.Question = s.Question
	v.RunID = s.RunID
	v.Transcript = s.Transcript
	v.TranscriptMore = s.TranscriptMore
	v.SessionID = s.SessionID
	v.Revision = s.Revision
	v.Phase = s.Phase
	v.Workflow = s.Workflow
	v.Busy = s.Busy
	v.WorkLabel = s.WorkLabel
	v.WorkStartedAt = s.WorkStartedAt
	v.Closing = s.Closing
	v.Settings = s.Settings
	v.SandboxRuntime = s.SandboxRuntime
	v.Usage = s.Usage
	v.ContextUsedTokens = s.ContextUsedTokens
	v.SystemTokens = s.SystemTokens
	v.ToolsTokens = s.ToolsTokens
	v.MessagesTokens = s.MessagesTokens
	v.PendingInputs = s.PendingInputs
	v.Approval = s.Approval
	v.Plan = s.Plan
	v.Tasks = s.Tasks
	v.LastTurn = s.LastTurn
}

// State returns the ordinary-update projection without copying history or catalog.
func (v SessionView) State() SessionState {
	return SessionState{
		HistoryLength:     v.HistoryLength,
		OutstandingWork:   v.OutstandingWork,
		Goal:              v.Goal,
		Question:          v.Question,
		RunID:             v.RunID,
		Transcript:        v.Transcript,
		TranscriptMore:    v.TranscriptMore,
		SessionID:         v.SessionID,
		Revision:          v.Revision,
		Phase:             v.Phase,
		Workflow:          v.Workflow,
		Busy:              v.Busy,
		WorkLabel:         v.WorkLabel,
		WorkStartedAt:     v.WorkStartedAt,
		Closing:           v.Closing,
		Settings:          v.Settings,
		SandboxRuntime:    v.SandboxRuntime,
		Usage:             v.Usage,
		ContextUsedTokens: v.ContextUsedTokens,
		SystemTokens:      v.SystemTokens,
		ToolsTokens:       v.ToolsTokens,
		MessagesTokens:    v.MessagesTokens,
		PendingInputs:     v.PendingInputs,
		Approval:          v.Approval,
		Plan:              v.Plan,
		Tasks:             v.Tasks,
		LastTurn:          v.LastTurn,
	}
}
