package tools

import (
	"strings"
	"testing"
)

func TestGitReadToolsReportNonRepositoryFailure(t *testing.T) {
	for _, tool := range []interface {
		Run(*Context) (string, error)
	}{NewGitStatusTool(), NewGitDiffTool(), NewGitLogTool()} {
		ctx := gitCtx(t.TempDir())
		out, err := tool.Run(ctx)
		if err == nil || !strings.Contains(strings.ToLower(out), "not a git repository") {
			t.Errorf("%T reported success for failed git: %q, %v", tool, out, err)
		}
		_ = ctx.Resources.Close()
	}
}
