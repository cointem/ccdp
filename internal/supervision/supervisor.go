package supervision

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"ccdp/internal/protocol"
)

// Supervisor owns the directory, admission capacity and join boundary. R is an
// opaque, comparable run handle; this component never inspects a session or
// invokes application callbacks while holding its mutex. Snapshots copy handles
// so callers can communicate with runtimes after releasing the directory lock.
type Supervisor[R comparable] struct {
	mu                                               sync.Mutex
	children                                         map[protocol.SessionID]R
	runs                                             map[protocol.RunID]R
	admissions                                       map[protocol.SessionID]reservation[R]
	live                                             map[R]bool // value marks a shared workspace writer
	active, workers, background                      int
	closed, stopping, workspaceMutation, interactive bool
	changed                                          chan struct{}
	work                                             map[protocol.SessionID]bool
	outstanding                                      atomic.Int64
}
type reservation[R comparable] struct {
	run    R
	id     protocol.RunID
	shared bool
}

func (s *Supervisor[R]) init() {
	if s.children != nil {
		return
	}
	s.children = make(map[protocol.SessionID]R)
	s.runs = make(map[protocol.RunID]R)
	s.admissions = make(map[protocol.SessionID]reservation[R])
	s.live = make(map[R]bool)
	s.changed = make(chan struct{})
}
func (s *Supervisor[R]) notify() { close(s.changed); s.changed = make(chan struct{}) }
func (s *Supervisor[R]) Reserve(row protocol.ChildSession, run, previous R) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	var zero R
	_, pending := s.admissions[row.SessionID]
	shared := row.WorkspaceMode == "shared" && row.Role == "worker"
	if s.closed || s.stopping || s.active >= 128 || len(s.runs)+len(s.admissions) >= 8192 ||
		(previous == zero && len(s.children)+len(s.admissions) >= 4096) ||
		(s.workspaceMutation && shared) || s.children[row.SessionID] != previous || pending {
		return errors.New("supervisor closed, child capacity reached, or session changed during preparation")
	}
	s.admissions[row.SessionID] = reservation[R]{run, row.Run.ID, shared}
	s.live[run] = shared
	s.active++
	s.workers++
	s.notify()
	return nil
}
func (s *Supervisor[R]) FinishAdmission(id protocol.SessionID, run R, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	pending, ok := s.admissions[id]
	if !ok || pending.run != run {
		panic("supervision: unknown admission")
	}
	delete(s.admissions, id)
	if err == nil {
		s.children[id] = run
		s.runs[pending.id] = run
	} else {
		delete(s.live, run)
		s.active--
		s.workers--
	}
	s.notify()
}
func (s *Supervisor[R]) Settled(run R) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if _, ok := s.live[run]; ok {
		delete(s.live, run)
		s.active--
		s.notify()
	}
}
func (s *Supervisor[R]) WorkerDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.workers--
	s.notify()
}
func (s *Supervisor[R]) Restore(row protocol.ChildSession, run R, current bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.runs[row.Run.ID] = run
	if current {
		s.children[row.SessionID] = run
	}
}
func (s *Supervisor[R]) Current(id protocol.SessionID) R {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.children[id]
}
func (s *Supervisor[R]) Run(id protocol.RunID) R { s.mu.Lock(); defer s.mu.Unlock(); return s.runs[id] }
func (s *Supervisor[R]) Lookup(id protocol.SessionID) (R, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero R
	if r, ok := s.children[id]; ok {
		return r, nil
	}
	var match R
	found := false
	for key, r := range s.children {
		if id != "" && strings.HasPrefix(string(key), string(id)) {
			if found {
				return zero, errors.New("ambiguous session id")
			}
			match = r
			found = true
		}
	}
	if !found {
		return zero, errors.New("child session not found in this root")
	}
	return match, nil
}
func (s *Supervisor[R]) Children() []R {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]R, 0, len(s.children))
	for _, r := range s.children {
		out = append(out, r)
	}
	return out
}
func (s *Supervisor[R]) Runs() []R {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]R, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out
}
func (s *Supervisor[R]) freeze(closeAll, exclusive bool) ([]R, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if exclusive && s.stopping {
		return nil, errors.New("tree stop already in progress")
	}
	if closeAll {
		s.closed = true
	} else {
		s.stopping = true
	}
	out := make([]R, 0, len(s.live))
	for r := range s.live {
		out = append(out, r)
	}
	s.notify()
	return out, nil
}
func (s *Supervisor[R]) CloseAdmission() []R                       { runs, _ := s.freeze(true, false); return runs }
func (s *Supervisor[R]) StopAdmission(exclusive bool) ([]R, error) { return s.freeze(false, exclusive) }
func (s *Supervisor[R]) ReopenAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if !s.closed {
		s.stopping = false
	}
	s.notify()
}
func (s *Supervisor[R]) StartBackground() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.closed {
		return false
	}
	s.background++
	return true
}
func (s *Supervisor[R]) BackgroundDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background--
	s.notify()
}
func (s *Supervisor[R]) Wait(ctx context.Context, includeBackground bool) error {
	for {
		s.mu.Lock()
		s.init()
		n := s.workers
		if includeBackground {
			n += s.background
		}
		changed := s.changed
		s.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (s *Supervisor[R]) ReserveWorkspace() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.workspaceMutation {
		return nil, errors.New("another workspace operation is running")
	}
	for _, shared := range s.live {
		if shared {
			return nil, errors.New("stop or wait for shared worker agents before changing workspace state")
		}
	}
	s.workspaceMutation = true
	return func() { s.mu.Lock(); s.workspaceMutation = false; s.mu.Unlock() }, nil
}
func (s *Supervisor[R]) SetInteractive(enabled bool) {
	s.mu.Lock()
	s.interactive = enabled
	s.mu.Unlock()
}
func (s *Supervisor[R]) Interactive() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.interactive }
func (s *Supervisor[R]) ProjectWork(row protocol.ChildSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := row.Run.Active() || row.DeliveryPending
	if s.work[row.SessionID] == active {
		return
	}
	if active {
		if s.work == nil {
			s.work = make(map[protocol.SessionID]bool)
		}
		s.work[row.SessionID] = true
		s.outstanding.Add(1)
	} else {
		delete(s.work, row.SessionID)
		s.outstanding.Add(-1)
	}
}
func (s *Supervisor[R]) Outstanding() int64 { return s.outstanding.Load() }

// Forget removes an archived directory entry. Active reservations remain owned
// by their worker and must finish before the session can be retired.
func (s *Supervisor[R]) Forget(id protocol.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.children[id]
	if !ok {
		return
	}
	if _, active := s.live[run]; active {
		return
	}
	delete(s.children, id)
	for key, r := range s.runs {
		if r == run {
			delete(s.runs, key)
		}
	}
}
