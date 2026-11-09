package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessPagesUseActualEncodedBudget(t *testing.T) {
	for _, text := range []string{strings.Repeat("ordinary log line\n", 1000), strings.Repeat("\x00<>&\"\\中文\n", 5000)} {
		path := filepath.Join(t.TempDir(), "output.log")
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		mp := &managedProcess{logPath: path, logSize: int64(len(text)), total: int64(len(text)), exited: true, done: closedProcessDone()}
		ctx := freshCtx(t)
		ctx.OutputLimit = 32000
		manager := ctx.Resources.Processes
		id, err := manager.newID()
		if err != nil {
			t.Fatal(err)
		}
		manager.procs[id] = mp
		defer delete(manager.procs, id) // synthetic process has no child to stop
		var recovered strings.Builder
		for offset := 0; offset < len(text); {
			out, err := runToolData(t, NewProcessTool(), ctx, map[string]any{"id": id, "action": "read", "offset": offset, "limit_bytes": 51200})
			page := decodeProcess(t, out, err)
			if len(out) > ctx.OutputLimit || page.Next <= offset {
				t.Fatalf("invalid page size/progress: bytes=%d offset=%d next=%d", len(out), offset, page.Next)
			}
			if page.Truncated && !strings.Contains(page.Text(), fmt.Sprintf("action=read offset=%d", page.Next)) {
				t.Fatal("missing exact continuation", page.Text())
			}
			if len(text) < 20000 && (page.Truncated || page.Output != text) {
				t.Fatal("ordinary output was needlessly truncated")
			}
			recovered.WriteString(page.Output)
			offset = page.Next
		}
		if recovered.String() != text {
			t.Fatal("pagination lost output across escaped/UTF-8 boundaries")
		}
	}
}

func TestProcessLostOutputDoesNotOfferUnavailableContinuation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	mp := &managedProcess{logPath: path, logSize: 5, total: 15, exited: true, done: closedProcessDone()}
	out, err := mp.view(&Context{}, "p.test", 0, 51200)
	page := decodeProcess(t, out, err)
	if page.Output != "saved" || !page.Truncated || !strings.Contains(page.Warning, "10 bytes were not retained") || strings.Contains(page.Warning, "More retained output") {
		t.Fatalf("lost output presented as another page: %+v", page)
	}
}

func TestRunningProcessDoesNotClaimOutputWasTruncated(t *testing.T) {
	mp := &managedProcess{}
	out, err := mp.view(&Context{Context: context.Background()}, "p.test", 0, 51200)
	page := decodeProcess(t, out, err)
	if page.Status != "running" || page.Truncated {
		t.Fatal("running status is not evidence of omitted output")
	}
}
