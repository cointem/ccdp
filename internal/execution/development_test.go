package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/sandbox"
)

func TestPreparedDevelopmentToolchains(t *testing.T) {
	c := NewController(t.TempDir() + "/runtime")
	defer c.Close()
	root := t.TempDir()
	policy := sandbox.New(root)
	policy.ExecutorID = c.ID
	policy.LocalDevelopmentNetwork = true
	scratch := t.TempDir()
	policy.AddScratchDir(scratch)
	fixtures := map[string]string{
		"go.mod": "module example.com/ccdp-sandbox-test\n\ngo 1.27.0\n",
		"smoke_test.go": `package smoke
import (
	"fmt"
	"net"
	"os/exec"
	"syscall"
	"testing"
)
func TestLocal(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	l.Close()
}
func TestChildSignals(t *testing.T) {
	for _, detached := range []bool{false, true} {
		for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			t.Run(fmt.Sprintf("detached=%t/signal=%d", detached, signal), func(t *testing.T) {
				child := exec.Command("/bin/sleep", "2")
				child.SysProcAttr = &syscall.SysProcAttr{Setsid: detached}
				if err := child.Start(); err != nil { t.Fatal(err) }
				// A finite lifetime also reclaims detached children under a broken policy.
				defer func() { child.Process.Kill(); child.Wait() }()
				if err := child.Process.Signal(syscall.Signal(0)); err != nil { t.Fatal(err) }
				if signal == syscall.SIGKILL {
					if err := child.Process.Kill(); err != nil { t.Fatal(err) }
				} else if err := child.Process.Signal(signal); err != nil { t.Fatal(err) }
				if err := child.Wait(); err == nil { t.Fatal("child exited without a signal") }
				status := child.ProcessState.Sys().(syscall.WaitStatus)
				if !status.Signaled() || status.Signal() != signal { t.Fatalf("exit status: %v", status) }
			})
		}
	}
}
`,
		"package.json": `{"name":"sandbox-smoke","version":"1.0.0","scripts":{"test":"node smoke.cjs"}}`,
		"smoke.cjs":    `require('http').createServer((q,r)=>r.end('ok')).listen(0,'127.0.0.1',function(){ console.log('node-ok'); this.close(); });`,
	}
	for path, data := range fixtures {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ name, command string }{
		{"go", "go test ./..."},
		{"node", "node smoke.cjs"},
		{"npm", "npm install --package-lock-only --ignore-scripts --offline --no-audit --no-fund && npm ci --offline --no-audit --no-fund && npm test"},
		{"pnpm", "pnpm install --offline --ignore-scripts && pnpm test"},
		{"python3", "python3 -m venv .venv && .venv/bin/python -c 'import socket; s=socket.socket(); s.bind((\"127.0.0.1\",0)); print(\"python-ok\"); s.close()' && .venv/bin/pip --version"},
		{"uv", "uv venv --offline .uv-venv && uv pip list --python .uv-venv/bin/python --offline"},
		{"cargo", "cargo init --lib --name sandbox_smoke rust-fixture && cd rust-fixture && cargo test --offline"},
		{"git", "git init git-fixture && cd git-fixture && git status --porcelain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := exec.LookPath(test.name); err != nil {
				t.Skip("toolchain not installed")
			}
			result, err := Run(Request{Context: context.Background(), Command: test.command, Dir: root, Sandbox: policy, Timeout: 90 * time.Second})
			if err != nil || result.ExitCode != 0 || result.TimedOut {
				t.Fatalf("%s: %+v %v", test.name, result, err)
			}
		})
	}
}

func TestScratchRepairAndCacheLinkCannotEscape(t *testing.T) {
	c := NewController(t.TempDir() + "/runtime")
	defer c.Close()
	policy := sandbox.New(t.TempDir())
	policy.ExecutorID = c.ID
	scratch := filepath.Join(t.TempDir(), "owned")
	policy.AddScratchDir(scratch)
	env, cleanup, err := prepareProcessEnvironment(nil, policy.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if environmentValue(env, "TMPDIR") != policy.Snapshot().ScratchDirs[0] {
		t.Fatal("missing scratch replaced with ephemeral path")
	}
	cache := environmentValue(env, "XDG_CACHE_HOME")
	if cache == "" || strings.HasPrefix(cache, scratch+string(filepath.Separator)) {
		t.Fatal("development cache must outlive owner scratch")
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "must-not-create")
	if err := os.Symlink(outside, cache); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareProcessEnvironment(nil, policy.Snapshot()); err == nil {
		t.Fatal("host followed workload cache link")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("host wrote outside scratch")
	}
	state, _ := EnvironmentStatus(policy)
	if state != "degraded" {
		t.Fatalf("persistent preparation error left environment %s", state)
	}
}

func TestConfigurationCopiesExcludeCredentialsAndPreserveDevelopmentSettings(t *testing.T) {
	for _, test := range []struct{ kind, input, want string }{
		{"npm", "registry=https://registry.npmjs.org\n//registry.npmjs.org/:_authToken=secret\n", "registry"},
		{"git", "[user]\nname = Ada\nemail = ada@example.test\n[credential]\nhelper = !secret-helper\n[http]\nextraHeader = Authorization: secret\n", "Ada"},
		{"pip", "[global]\ntimeout=20\nindex-url=https://name:secret@example.test/simple\n", "timeout"},
		{"go", "GOPROXY=https://proxy.golang.org,direct\nGOPRIVATE=example.test\nGOAUTH=secret\n", "GOPRIVATE=example.test"},
	} {
		got := filterToolConfiguration(test.kind, test.input)
		if strings.Contains(got, "secret") || !strings.Contains(got, test.want) {
			t.Fatalf("%s configuration: %q", test.kind, got)
		}
	}
}
