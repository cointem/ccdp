package agent

import (
	"testing"

	"ccdp/internal/tools"
)

func TestGoalToolsAreSerialPlanCapabilities(t *testing.T) {
	for _, tool := range []tools.Tool{tools.NewCreateGoalTool(), tools.NewGetGoalTool(), tools.NewUpdateGoalTool()} {
		if !planAllowedImplementation(tool) {
			t.Errorf("%s unavailable in plan mode", tool.Name())
		}
		if readOnlyImplementation(tool) {
			t.Errorf("%s must not race with other tool invocations", tool.Name())
		}
	}
}
