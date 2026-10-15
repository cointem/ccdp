package execution

import (
	"context"
	"fmt"
	"io"
	"os"

	"ccdp/internal/executable"
)

// RunReadOnlySearch runs only the installed ripgrep executable, with the same
// no-network/no-workspace-write boundary as typed Git queries. Callers own
// pattern arguments and must supply already authorized file paths.
func RunReadOnlySearch(ctx context.Context, args []string, req Request, stdout io.Writer, readRoot string) (Result, error) {
	path := executable.FindTrusted("rg", os.Getenv("PATH"))
	if path == "" {
		return Result{}, fmt.Errorf("trusted ripgrep executable unavailable")
	}
	req.readOnlyRoot = readRoot
	req.stdoutSink = stdout
	req.readOnlyQuery = true // shared query-only process profile; no shell or helpers
	return RunArgv(ctx, append([]string{path}, args...), req)
}
