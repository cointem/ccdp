package execution

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
)

// Exercise the real profile, including its metadata-only private ancestors.
// Ordinary host tests do not encounter this boundary and missed the regression.
func TestRuntimePreparationWithinSandbox(t *testing.T) {
	workspace := t.TempDir()
	private := filepath.Join(workspace, "private")
	scratch := filepath.Join(private, "owner", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(workspace)
	policy.AddPrivateRoot(private)
	policy.AddScratchDir(scratch)
	profile, err := policy.Profile()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sandbox.BackendPath, "-p", profile, "--", os.Args[0], "-test.run=^TestSandboxedRuntimeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CCDP_TEST_RUNTIME_SCRATCH="+scratch)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed runtime: %v\n%s", err, output)
	}
}

func TestSandboxedRuntimeHelper(t *testing.T) {
	scratch := os.Getenv("CCDP_TEST_RUNTIME_SCRATCH")
	if scratch == "" {
		t.Skip("subprocess helper")
	}
	private := filepath.Dir(filepath.Dir(scratch))
	for _, path := range []string{private, filepath.Dir(scratch)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ancestor metadata unavailable: %v", err)
		}
		if _, err := os.ReadDir(path); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("ancestor listing must remain denied: %v", err)
		}
	}
	// Both existing and new owned roots must work without listing ancestors.
	for _, path := range []string{scratch, filepath.Join(scratch, "runtime", "child")} {
		prepared, err := runtimepath.EnsurePrivate(path)
		if err != nil {
			t.Fatalf("prepare authorized private root: %v", err)
		}
		if err := os.WriteFile(filepath.Join(prepared, "data"), []byte("ok"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtimepath.EnsurePrivate(filepath.Join(private, "other-owner")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("runtime preparation escaped scratch: %v", err)
	}
	// Even an authorized runtime root cannot make nested Seatbelt available.
	// Report that cause before creating runtime state or blaming directory ACLs.
	root := filepath.Join(scratch, "executor-root")
	c := NewController(root)
	defer c.Close()
	_, err := NewEnvironment(c.ID)
	if !errors.Is(err, sandbox.ErrBackendUnavailable) || !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("nested backend failure lost: %v", err)
	}
	if !strings.Contains(err.Error(), "host terminal") || strings.Contains(err.Error(), "private runtime preparation") {
		t.Fatalf("nested failure is not actionable: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unavailable backend prepared runtime state: %v", err)
	}
}
