package tui

import (
	"context"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type recoveryDirectory struct {
	protocol.SessionDirectory
	clients map[protocol.SessionID]protocol.SessionClient
}

func (d recoveryDirectory) OpenReader(_ context.Context, id protocol.SessionID) (protocol.SessionClient, error) {
	return d.clients[id], nil
}

func recoverTestOperations(t *testing.T, m Model) Model {
	t.Helper()
	recover := m.recoverOperationsCmd()
	if recover == nil {
		t.Fatal("no completion recovery")
	}
	next, _ := m.Update(recover())
	return modelValue(t, next)
}

func TestScheduledOperationRecoveredAfterNavigation(t *testing.T) {
	a := newLifecycleTestAgent(t)
	defer a.Close()
	m := NewWithResume(a, false)
	snapshot, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.applySnapshot(snapshot)
	child := &recordingClient{snapshot: protocolSnapshot("child")}
	m.installSessionRouting(recoveryDirectory{clients: map[protocol.SessionID]protocol.SessionClient{
		snapshot.SessionID: a, "child": child,
	}})
	m.width, m.height = 100, 30
	m.layout()
	_, submit := m.runCommand("/cd " + t.TempDir())
	if submit == nil {
		t.Fatal("workspace command unavailable")
	}
	msg := submit().(commandReceiptMsg)
	if msg.receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("receipt=%+v", msg.receipt)
	}
	next, _ := m.Update(msg)
	m = modelValue(t, next)
	m = switchRoutingTest(t, m, "child")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		result, err := a.ReadOperation(ctx, msg.receipt.CommandID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete {
			if result.Status != "success" {
				t.Fatalf("result=%+v", result)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	m = switchRoutingTest(t, m, "root")
	m = recoverTestOperations(t, m)
	defer m.Close()
	op, _ := m.operation(msg.receipt.CommandID)
	if op.Active() || m.activity.Active() || m.busy {
		t.Fatalf("completed operation still active: operation=%+v activity=%+v", op, m.activity)
	}
}

type recoveryClient struct {
	*recordingClient
	inputs map[protocol.InputID]protocol.InputResult
	result protocol.OperationResult
}

func (c *recoveryClient) ReadInput(_ context.Context, id protocol.InputID) (protocol.InputResult, error) {
	return c.inputs[id], nil
}
func (c *recoveryClient) ReadOperation(context.Context, protocol.CommandID) (protocol.OperationResult, error) {
	return c.result, nil
}

func TestInputRecoveryRetainsEachTurnOutcomeAfterNavigation(t *testing.T) {
	m, directory := routingTestModel()
	source := &recoveryClient{recordingClient: directory.clients["root"], inputs: map[protocol.InputID]protocol.InputResult{
		"first":     {ID: "first", Delivered: true, TurnID: "turn-1", Status: protocol.TurnFailed, Error: "first turn failed"},
		"second":    {ID: "second", Delivered: true, TurnID: "turn-2", Status: protocol.TurnSucceeded},
		"third":     {ID: "third", Delivered: true, TurnID: "turn-3"},
		"queued":    {ID: "queued"},
		"cancelled": {ID: "cancelled", Status: protocol.TurnCancelled, Error: "cancelled while queued"},
	}}
	m.client = source
	for _, id := range []string{"first", "second", "third", "queued", "cancelled"} {
		cmd := protocol.NewSubmitInput(protocol.CommandID(id), "root", protocol.InputID(id), id, protocol.InputFollowup)
		m.submitCommand(cmd, "message submitted")
		m.applyReceipt(protocol.Receipt{CommandID: cmd.ID, SessionID: "root", Status: protocol.ReceiptScheduled}, "")
	}
	// The read may return after another navigation; settle the originating view.
	recover := m.recoverOperationsCmd()
	m = switchRoutingTest(t, m, "child")
	next, _ := m.Update(recover())
	m = modelValue(t, next)
	if len(m.operations) != 0 {
		t.Fatal("root operation recovery entered the child")
	}
	m = switchRoutingTest(t, m, "root")
	defer m.Close()
	if m.operations["first"].State != OperationFailed || m.operations["first"].Error == "" ||
		m.operations["second"].State != OperationApplied {
		t.Fatalf("turn outcomes changed: %+v", m.operations)
	}
	if !m.operations["third"].Active() || m.operations["third"].Recoverable {
		t.Fatal("delivered running input was completed or retained as an unsent draft")
	}
	if !m.operations["queued"].Recoverable || m.pendingSubmissionCount() != 1 {
		t.Fatal("queued input was consumed without delivery")
	}
	if op := m.operations["cancelled"]; op.State != OperationCancelled || op.TurnID != "" || op.Recoverable {
		t.Fatalf("queued cancellation was treated as a delivered turn: %+v", op)
	}
}

func TestRecoveryRestoresReportAfterReceiptAndDoesNotResurrectOperation(t *testing.T) {
	m, directory := routingTestModel()
	source := &recoveryClient{recordingClient: directory.clients["root"],
		result: protocol.OperationResult{ID: "export", Complete: true, Status: "success", Output: "saved export"}}
	m.client = source
	m.submitCommand(protocol.Command{ID: "export", Type: protocol.CommandExport, Export: &protocol.ExportCommand{}}, "export")
	m.applyReceipt(protocol.Receipt{CommandID: "export", SessionID: "root", Status: protocol.ReceiptApplied}, "")
	recover := m.recoverOperationsCmd()
	if recover == nil {
		t.Fatal("missed report was not queried after its terminal receipt")
	}
	msg := recover()
	next, _ := m.Update(msg)
	m = modelValue(t, next)
	next, _ = m.Update(msg)
	m = modelValue(t, next)
	defer m.Close()
	recoveredReports := 0
	for _, report := range m.reports {
		if report.reportID == "export" {
			recoveredReports++
			if report.text != "saved export" {
				t.Fatalf("recovered report=%+v", report)
			}
		}
	}
	if m.operations["export"].State != OperationApplied || recoveredReports != 1 {
		t.Fatalf("report/operation recovery=%+v %+v", m.reports, m.operations)
	}
}

func TestOperationRecoverySurvivesWorkspaceState(t *testing.T) {
	m, directory := routingTestModel()
	defer m.Close()
	m.client = &recoveryClient{recordingClient: directory.clients["root"],
		result: protocol.OperationResult{ID: "export", Complete: true, Status: "success", Output: "saved export"},
		inputs: map[protocol.InputID]protocol.InputResult{
			"input": {ID: "input", Delivered: true, TurnID: "turn-1", Status: protocol.TurnFailed, Error: "turn failed"},
		}}
	m.submitCommand(protocol.Command{ID: "export", Type: protocol.CommandExport, Export: &protocol.ExportCommand{}}, "export")
	m.submitCommand(protocol.NewSubmitInput("input", "root", "input", "message", protocol.InputFollowup), "message")
	recovery := m.recoverOperationsCmd()
	if recovery == nil {
		t.Fatal("completion recovery unavailable")
	}
	recovered := recovery()
	// A queued /cd publishes its authoritative State while the reconnect
	// query result is waiting to return to the UI.
	m = applyTestWorkspaceState(t, m)
	next, _ := m.Update(recovered)
	m = modelValue(t, next)
	if m.operations["export"].State != OperationApplied || m.operations["input"].State != OperationFailed {
		t.Fatalf("workspace update invalidated completion recovery: %+v", m.operations)
	}
}

func applyTestWorkspaceState(t *testing.T, m Model) Model {
	t.Helper()
	previousGeneration := m.reportGeneration
	state := m.snapshot.State()
	state.Settings.Workspace = t.TempDir()
	next, _ := m.Update(watchUpdateMsg{sessionID: protocol.SessionID(m.sessionID),
		generation: m.watchGeneration, sub: m.subscription,
		update: protocol.Update{Type: protocol.UpdateState, State: &state}})
	m = modelValue(t, next)
	if m.reportGeneration == previousGeneration {
		t.Fatal("test did not publish a workspace change")
	}
	return m
}
