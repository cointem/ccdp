package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// TestSeatbeltInstalledToolchains smoke-tests only tools installed on this
// host. Every child uses the production runner, and all writable scratch/cache
// state is confined to the temporary workspace.
func TestSeatbeltInstalledToolchains(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CCDP_SEATBELT_INTEGRATION") != "1" {
		t.Skip("opt-in macOS Seatbelt integration test")
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc main() { println(\"seatbelt-go-ok\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb := sandbox.New(workspace)
	// Reuse the already-authorized workspace as the child HOME, TMPDIR, and
	// tool cache root. The integration policy does not grant the host temp root.
	sb.SetScratchDir(workspace)

	tools := []struct {
		name string
		args []string
		want string
		env  []string
	}{
		{name: "go", args: []string{"run", "main.go"}, want: "seatbelt-go-ok", env: []string{
			"GOENV=off", "GOTOOLCHAIN=local", "GO111MODULE=off", "CGO_ENABLED=0", "GOPROXY=off",
		}},
		{name: "node", args: []string{"-e", "console.log('seatbelt-node-ok')"}, want: "seatbelt-node-ok"},
		{name: "python3", args: []string{"-B", "-S", "-c", "print('seatbelt-python-ok')"}, want: "seatbelt-python-ok"},
	}
	installed := 0
	for _, tool := range tools {
		tool := tool
		t.Run(tool.name, func(t *testing.T) {
			path, err := exec.LookPath(tool.name)
			if err != nil {
				if tool.name == "go" {
					t.Fatalf("Go is required for this test run: %v", err)
				}
				t.Skipf("%s is not installed", tool.name)
			}
			installed++

			env := append([]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}, tool.env...)
			result, runErr := execution.RunArgv(context.Background(), append([]string{path}, tool.args...), execution.Request{
				Context:     context.Background(),
				Dir:         workspace,
				Env:         env,
				Sandbox:     sb,
				Timeout:     60 * time.Second,
				OutputLimit: 64 * 1024,
			})
			if runErr != nil || result.ExitCode != 0 || !strings.Contains(result.Output, tool.want) {
				t.Fatalf("%s under Seatbelt: exit=%d err=%v output=%q", tool.name, result.ExitCode, runErr, result.Output)
			}
		})
	}
	if installed == 0 {
		t.Fatal("no installed toolchain executable was smoke-tested")
	}
	if sb.NetworkAllowed() {
		t.Fatal("toolchain smoke unexpectedly authorized network access")
	}
}

func TestSeatbeltRustcUsesPrivateScratch(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CCDP_SEATBELT_INTEGRATION") != "1" {
		t.Skip("opt-in macOS Seatbelt integration test")
	}
	// Use an installed compiler directly: the rustup shim resolves its config
	// under HOME, which is intentionally isolated for sandboxed commands.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	compilers, err := filepath.Glob(filepath.Join(home, ".rustup", "toolchains", "*", "bin", "rustc"))
	if err != nil || len(compilers) == 0 {
		t.Skip("no installed rustup compiler")
	}
	workspace, scratch := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.rs"), []byte("fn main() { println!(\"seatbelt-rust-ok\"); }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sb := sandbox.New(workspace)
	sb.SetScratchDir(scratch)
	command := fmt.Sprintf("%q --edition 2021 main.rs -o \"$TMPDIR/rust-test\" && \"$TMPDIR/rust-test\"", compilers[0])
	result, err := execution.Run(execution.Request{Context: context.Background(), Command: command, Dir: workspace, Sandbox: sb, Timeout: 30 * time.Second})
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Output, "seatbelt-rust-ok") {
		t.Fatalf("Rust compiler cannot use private scratch: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(scratch, "rust-test")); err != nil {
		t.Fatalf("compiler did not honor private TMPDIR output: %v", err)
	}
}
