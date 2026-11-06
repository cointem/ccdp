package agent

import (
	"testing"

	"ccdp/internal/tools"
)

func TestGoalToolsAreSerialCapabilities(t *testing.T) {
	for _, tool := range []tools.Tool{tools.NewCreateGoalTool(), tools.NewGetGoalTool(), tools.NewUpdateGoalTool()} {
		if readOnlyImplementation(tool) {
			t.Errorf("%s must not race with other tool invocations", tool.Name())
		}
	}
}
