package agent

func testSupervisor(root *Agent, runs ...*managedRun) *SessionSupervisor {
	s := &SessionSupervisor{root: root}
	for _, r := range runs {
		s.directory.Restore(r.fact.Child, r, true)
	}
	return s
}
