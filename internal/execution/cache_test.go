package execution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/sandbox"
)

func TestDevelopmentCachesAreScopedAndSurviveControllers(t *testing.T) {
	root, project := t.TempDir()+"/runtime", t.TempDir()
	controller := NewController(root)
	defer controller.Close()
	prepare := func(c *Controller, policy *sandbox.Sandbox) string {
		t.Helper()
		policy.ExecutorID = c.ID
		policy.AddScratchDir(t.TempDir())
		env, cleanup, err := prepareProcessEnvironment(nil, policy)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		return environmentValue(env, "XDG_CACHE_HOME")
	}
	base := sandbox.New(project)
	cache := prepare(controller, base)
	marker := filepath.Join(cache, "cached-artifact")
	if err := os.WriteFile(marker, []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	controller.Close()
	next := NewController(root)
	defer next.Close()
	if got := prepare(next, sandbox.New(project)); got != cache {
		t.Fatalf("cache changed across controller restart: %s != %s", got, cache)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "saved" {
		t.Fatal("cache was removed on close", err)
	}
	for _, name := range []string{"workspace", "network", "read_deny", "write_grant"} {
		t.Run(name, func(t *testing.T) {
			policy := sandbox.New(project)
			switch name {
			case "workspace":
				policy = sandbox.New(t.TempDir())
			case "network":
				policy.AllowNetwork = !base.AllowNetwork
			case "read_deny":
				policy.AddDisallowedDir(filepath.Join(project, "secret"))
			case "write_grant":
				policy.AddDir(t.TempDir())
			}
			if got := prepare(next, policy); got == cache {
				t.Fatal("different access scope reused cache")
			}
			if _, err := policy.ResolveRead(marker); err == nil {
				t.Fatal("file policy exposed another cache scope")
			}
			result, err := Run(Request{Context: context.Background(), Sandbox: policy, Dir: policy.Workspace,
				Command: fmt.Sprintf("cat %s", shellQuote(marker))})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode == 0 {
				t.Fatal("Seatbelt exposed another cache scope")
			}
		})
	}
}
