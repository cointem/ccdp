package execution

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccdp/internal/sandbox"
)

func TestSandboxedCommandsUsePolicyProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "public.invalid" {
			t.Errorf("unexpected upstream target %s", r.URL)
		}
		fmt.Fprint(w, "through policy proxy")
	}))
	defer upstream.Close()
	root := t.TempDir()
	policy := sandbox.New(root)
	policy.AllowNetwork = true
	policy.NetworkProxy = upstream.URL
	req := Request{Context: context.Background(), Dir: root, Sandbox: policy, Timeout: 5 * time.Second}
	req.Argv = []string{"/usr/bin/curl", "--fail", "--silent", "--show-error", "--max-time", "2", "http://public.invalid/"}
	res, err := Run(req)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "through policy proxy") {
		t.Fatalf("proxied command: %+v %v", res, err)
	}
	// Explicitly bypassing the environment must not reach the trusted proxy.
	req.Argv = []string{"/usr/bin/curl", "--noproxy", "*", "--silent", "--show-error", "--max-time", "1", upstream.URL}
	res, err = Run(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("command bypassed gateway to upstream loopback port")
	}
	// Nor may a child opt out to direct public TCP, even with a fixed IP.
	req.Argv = []string{"/usr/bin/curl", "--noproxy", "*", "--silent", "--show-error", "--max-time", "1", "http://1.1.1.1/"}
	res, err = Run(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("command bypassed gateway to direct public TCP")
	}
	if !policy.NetworkAllowed() || len(policy.Snapshot().LocalServices) != 0 {
		t.Fatal("invocation mutated session policy")
	}
}

func TestDisabledNetworkDoesNotCreateProxy(t *testing.T) {
	policy := sandbox.New(t.TempDir())
	policy.NetworkProxy = "invalid proxy"
	env, cleanup, err := prepareProcessEnvironment([]string{"HTTP_PROXY=http://127.0.0.1:99"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, entry := range env {
		if strings.Contains(strings.ToUpper(entry), "PROXY=") {
			t.Fatalf("proxy inherited with network disabled: %s", entry)
		}
	}
}
