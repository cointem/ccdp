package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func TestResumeCallbackAfterChildNavigation(t *testing.T) {
	m, _ := routingTestModel()
	source := newLifecycleTestAgent(t)
	defer source.Close()
	m.ag = source
	// Delay the real async opener until after navigation. A failed open must
	// release the shared submission gate just like a successful completion.
	resume := m.openResumeSession("missing-session")
	if resume == nil || !m.resumePending {
		t.Fatal("resume was not admitted")
	}
	m = switchRoutingTest(t, m, "child")
	next, _ := m.Update(resume())
	m = modelValue(t, next)
	m = switchRoutingTest(t, m, "root")
	defer m.Close()
	m.textarea.SetValue("message after the completed resume attempt")
	_, submit := m.submit()
	if m.resumePending || submit == nil {
		t.Fatalf("resumePending=%v submitAllowed=%v", m.resumePending, submit != nil)
	}
}

func TestResumeCandidateAfterChildNavigationIsClosed(t *testing.T) {
	m, _ := routingTestModel()
	source := newLifecycleTestAgent(t)
	defer source.Close()
	candidate := newLifecycleTestAgent(t)
	defer candidate.Close()
	m.ag = source
	_ = m.openResumeSession("saved-session")
	task := m.resumeTask
	task.begin()
	task.finish(candidate)
	msg := resumeOpenedMsg{source: source, candidate: candidate, task: task,
		expectedSessionID: m.sessionID, expectedGeneration: m.watchGeneration}

	m = switchRoutingTest(t, m, "child")
	next, closeCandidate := m.handleResumeOpened(msg)
	m = modelValue(t, next)
	if closeCandidate == nil {
		t.Fatal("stale opened candidate was not closed")
	}
	_ = closeCandidate()
	assertAgentClosed(t, candidate)
	if m.CurrentAgent() != source || m.sessionID != "child" {
		t.Fatal("stale resume replaced the current runtime or child view")
	}
	m = switchRoutingTest(t, m, "root")
	defer m.Close()
	if m.resumePending {
		t.Fatal("root retained the completed resume gate")
	}
}

func TestCloseAfterChildNavigationJoinsResumeTask(t *testing.T) {
	m, _ := routingTestModel()
	candidate := newLifecycleTestAgent(t)
	defer candidate.Close()
	retired := newLifecycleTestAgent(t)
	defer retired.Close()
	task := &resumeTaskState{done: make(chan struct{})}
	task.begin()
	m.resumeTask, m.resumePending = task, true
	m.retiredAgents = append(m.retiredAgents, retired)
	m = switchRoutingTest(t, m, "child")

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("child view closed before the resume task completed: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	task.finish(candidate)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join the resume task")
	}
	assertAgentClosed(t, candidate)
	assertAgentClosed(t, retired)
}

type navigationQueryClient struct {
	*recordingClient
	err error
}

func (c *navigationQueryClient) Query(ctx context.Context, kind protocol.QueryKind) (protocol.QueryReport, error) {
	if err := ctx.Err(); err != nil {
		return protocol.QueryReport{}, err
	}
	return protocol.QueryReport{Kind: kind, Text: "saved query result"}, c.err
}

func TestQueryResultAfterChildNavigation(t *testing.T) {
	for _, result := range []string{"success", "cancelled", "error"} {
		t.Run(result, func(t *testing.T) {
			m, directory := routingTestModel()
			client := &navigationQueryClient{recordingClient: directory.clients["root"]}
			if result == "error" {
				client.err = errors.New("query unavailable")
			}
			m.client = client
			query := m.runQuery(protocol.QueryGitHub, "GitHub status requested")
			id := m.latestOperation
			var msg queryReportMsg
			if result != "cancelled" {
				// The result is ready, but its UI delivery races navigation.
				msg = query().(queryReportMsg)
			}
			m = switchRoutingTest(t, m, "child")
			if result == "cancelled" {
				// Navigation cancels the observation context. The resulting
				// query error must still settle its original operation.
				msg = query().(queryReportMsg)
				if !errors.Is(msg.err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", msg.err)
				}
			}
			next, _ := m.Update(msg)
			m = modelValue(t, next)
			for _, report := range m.reports {
				if report.reportID == id.String() {
					t.Fatal("root query result entered the child transcript")
				}
			}
			m = switchRoutingTest(t, m, "root")
			defer m.Close()
			op, ok := m.operation(id)
			want := OperationApplied
			if result != "success" {
				want = OperationFailed
			}
			if !ok || op.State != want || op.Active() {
				t.Fatalf("operationKnown=%v state=%q active=%v", ok, op.State, op.Active())
			}
			found := false
			for _, report := range m.reports {
				if report.reportID == id.String() {
					found = true
					if result == "success" && report.text != "saved query result" {
						t.Fatalf("query result changed: %q", report.text)
					}
					if result != "success" && !strings.Contains(report.text, msg.err.Error()) {
						t.Fatalf("query failure missing: %q", report.text)
					}
				}
			}
			if !found {
				t.Fatal("query result was lost while its session was cached")
			}
		})
	}
}
