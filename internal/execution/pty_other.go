//go:build !darwin

package execution

import (
	"fmt"
	"os"
)

func openProcessPTY() (*os.File, *os.File, error) {
	return nil, nil, fmt.Errorf("TTY is not supported on this platform")
}
