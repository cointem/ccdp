package supervision

import (
	"ccdp/internal/protocol"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func directoryRow(id string, shared bool) protocol.ChildSession {
	row := protocol.ChildSession{SessionID: protocol.SessionID(id), Run: protocol.RunView{ID: protocol.RunID(id), Status: "queued"}}
	if shared {
		row.WorkspaceMode = "shared"
		row.Role = "worker"
	}
	return row
}
func TestConcurrentReservationsRespectCapacityAndRelease(t *testing.T) {
	var s Supervisor[string]
	var group sync.WaitGroup
	admitted := make(chan string, 256)
	for i := 0; i < 256; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			id := fmt.Sprint(i)
			if s.Reserve(directoryRow(id, false), id, "") == nil {
				admitted <- id
			}
		}(i)
	}
	group.Wait()
	close(admitted)
	if len(admitted) != 128 {
		t.Fatalf("admitted %d, want capacity 128", len(admitted))
	}
	for id := range admitted {
		s.FinishAdmission(protocol.SessionID(id), id, errors.New("prepare failed"))
	}
	if err := s.Wait(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(directoryRow("retry", false), "retry", ""); err != nil {
		t.Fatal(err)
	}
	s.FinishAdmission("retry", "retry", nil)
	s.Settled("retry")
	s.WorkerDone()
}
func TestPendingAdmissionParticipatesInStopAndWorkspaceBarrier(t *testing.T) {
	var s Supervisor[string]
	row := directoryRow("child", true)
	if err := s.Reserve(row, "run", ""); err != nil {
		t.Fatal(err)
	}
	if s.Current("child") != "" {
		t.Fatal("uncommitted child became observable")
	}
	if release, err := s.ReserveWorkspace(); err == nil {
		release()
		t.Fatal("workspace mutation crossed pending shared worker")
	}
	runs, err := s.StopAdmission(true)
	if err != nil || len(runs) != 1 || runs[0] != "run" {
		t.Fatalf("pending cancellation=%v %v", runs, err)
	}
	if err := s.Reserve(directoryRow("later", false), "later", ""); err == nil {
		t.Fatal("stop allowed new admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if !errors.Is(s.Wait(ctx, false), context.DeadlineExceeded) {
		t.Fatal("join lost pending worker")
	}
	s.FinishAdmission("child", "run", nil)
	s.Settled("run")
	s.WorkerDone()
	s.ReopenAdmission()
	release, err := s.ReserveWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := s.Wait(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s.CloseAdmission()
	s.ReopenAdmission()
	if err := s.Reserve(directoryRow("later", false), "later", ""); err == nil {
		t.Fatal("closed directory reopened")
	}
}
func TestRestoreLateOldRunDoesNotReplaceContinuation(t *testing.T) {
	var s Supervisor[string]
	first := directoryRow("child", false)
	first.Run.ID = "old"
	next := first
	next.Run.ID = "new"
	s.Restore(first, "first", true)
	s.Restore(next, "second", true)
	s.Restore(first, "late", false)
	if s.Current("child") != "second" || s.Run("old") != "late" {
		t.Fatal("late receipt moved current run backwards")
	}
}
