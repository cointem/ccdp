package permissions

import "testing"

func TestGoalToolsRespectExplicitPolicyWithoutDiskApproval(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass} {
		for _, name := range []string{"CreateGoal", "GetGoal", "UpdateGoal"} {
			manager := NewManager(mode, Policy{})
			if decision, reason := manager.Check(name, nil, KindState); decision != DecisionAllow {
				t.Errorf("%s/%s denied: %s %s", mode, name, decision, reason)
			}
			manager = NewManager(mode, Policy{AlwaysDeny: []string{name}})
			if decision, _ := manager.Check(name, nil, KindState); decision != DecisionDeny {
				t.Errorf("%s/%s ignored explicit denial", mode, name)
			}
			if !KindState.PlanAllowed() {
				t.Errorf("%s is unavailable while planning", name)
			}
		}
	}
}
