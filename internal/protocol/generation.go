package protocol

import "fmt"

type SetGeneration struct {
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	Verbosity       *string `json:"verbosity,omitempty"`
}

// These are wire values; individual providers/models may support a subset.
func ValidateGeneration(effort, verbosity string) error {
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("invalid reasoning_effort %q (use none|minimal|low|medium|high|xhigh|max|ultra, or empty for the built-in default medium)", effort)
	}
	switch verbosity {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("invalid verbosity %q (use low|medium|high, or empty for provider default)", verbosity)
	}
	return nil
}
