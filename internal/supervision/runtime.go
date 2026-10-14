// Package supervision defines the capabilities used to manage child runs.
// Session construction and execution are supplied by the application layer.
package supervision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

// ChildFactory constructs a runtime from a run's frozen inputs. On failure it
// releases partially acquired session resources; on success the caller owns
// FinishRun/CloseContext. Each factory is scoped to one run.
type ChildFactory interface {
	Open(context.Context) (ChildRuntime, error)
}

// ChildRuntime owns its journal throughout FinishRun. A canceled waiter does
// not cancel finalization; retry the same run identity to observe its result.
type ChildRuntime interface {
	protocol.SessionClient
	SessionID() string
	AdmitMessage(protocol.Command) (protocol.Receipt, error)
	RunStatus() RuntimeStatus
	RunOutput(map[string]bool) string
	RunBudgetError() error
	PersistenceFailure() error
	RequestAdmitted(string) bool
	ReadRunRecords(context.Context) ([]session.Record, journal.ArtifactReader, error)
	Context() context.Context
	Done() <-chan struct{}
	CloseContext(context.Context) error
	FinishRun(context.Context, FinishRequest) (FinishResult, error)
}

// ResultCapture is a scoped workspace capability, not a parent runtime handle.
type ResultCapture interface {
	Capture(context.Context, string, *sandbox.Sandbox) (snapshotID string, err error)
}

type FinishRequest struct {
	Fact                     session.ChildRunRecorded
	Baseline                 protocol.UsageSnapshot
	Output, Error, SaveError string
	Outcome                  protocol.ExecutionOutcome
	Cancelled                bool
	Capture                  ResultCapture `json:"-"`
}

type FinishResult struct {
	Fact         session.ChildRunRecorded
	OutboxCursor session.Cursor
	Records      []session.Record
	Artifacts    journal.ArtifactAccess
}

// Freeze owns all request data and computes a stable identity for retries.
// The resource capability is pinned by the first admission, never replaced.
func (r FinishRequest) Freeze() (FinishRequest, string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return FinishRequest{}, "", err
	}
	var frozen FinishRequest
	if err := json.Unmarshal(data, &frozen); err != nil {
		return FinishRequest{}, "", err
	}
	frozen.Capture = r.Capture
	sum := sha256.Sum256(data)
	return frozen, hex.EncodeToString(sum[:]), nil
}

func (r FinishResult) Clone() (FinishResult, error) {
	data, err := json.Marshal(r.Fact)
	if err != nil {
		return FinishResult{}, err
	}
	var fact session.ChildRunRecorded
	if err := json.Unmarshal(data, &fact); err != nil {
		return FinishResult{}, err
	}
	records, err := session.CloneRecords(r.Records)
	if err != nil {
		return FinishResult{}, err
	}
	r.Fact, r.Records = fact, records
	return r, nil
}

// SetOutcome is shared by constructed and failed-to-construct runs. Time is
// explicit so an accepted finalization has one stable terminal fact.
func SetOutcome(fact *session.ChildRunRecorded, output, failure, saveError string, outcome protocol.ExecutionOutcome, cancelled bool, now time.Time) {
	run := &fact.Child.Run
	run.Status, run.Error, run.SaveError = "succeeded", failure, saveError
	if failure != "" {
		run.Status = "failed"
	}
	if saveError != "" {
		run.Error = ""
	}
	if outcome != "" && (failure == "" || saveError != "") {
		run.Status = outcome.RunStatus()
	}
	if cancelled {
		run.Status = "cancelled"
	}
	if run.FinishedAt.IsZero() {
		run.FinishedAt = now
	}
	run.Output = output
	fact.DeliveryID = "child-result-" + string(run.ID)
}

// RuntimeStatus contains only the live values needed by the run directory.
// Pointers and slices are owned by the returned snapshot.
type RuntimeStatus struct {
	Approval *protocol.ApprovalView
	Question *protocol.QuestionRequest
	Plan     *protocol.PlanView
	Usage    protocol.UsageSnapshot
	ToolUses int
}
