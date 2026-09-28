//go:build !darwin && !linux

package execution

import "fmt"

func waitProcessExit(pid int) error {
	return fmt.Errorf("process exit observation is unsupported on this platform")
}
