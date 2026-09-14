package sandbox

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestStrictProfileIntegration is opt-in because nested CI/Codex sandboxes
// commonly deny sandbox_apply even when /usr/bin/sandbox-exec exists. The
// escalation/manual run sets CCDP_STRICT_INTEGRATION=1 and uses only t.TempDir.
func TestStrictProfileIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CCDP_STRICT_INTEGRATION") != "1" {
		t.Skip("opt-in macOS sandbox integration test")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	additional := filepath.Join(workspace, "additional")
	scratch := filepath.Join(workspace, "scratch")
	blocked := filepath.Join(workspace, "blocked")
	for _, dir := range []string{additional, scratch, blocked} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret")
	const secretSentinel = "ccdp-strict-outside-read-sentinel-7f4b"
	if err := os.WriteFile(secret, []byte(secretSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(workspace, ModeStrict)
	s.AddDir(additional)
	s.SetScratchDir(scratch)
	s.AddDisallowedDir(blocked)

	run := func(command string) (stdout, stderr []byte, err error) {
		prepared, err := PrepareCommand(s, command)
		if err != nil {
			return nil, nil, err
		}
		child := exec.Command("/bin/sh", "-c", prepared)
		child.Dir = s.Workspace
		var out, diagnostic bytes.Buffer
		child.Stdout = &out
		child.Stderr = &diagnostic
		err = child.Run()
		return out.Bytes(), diagnostic.Bytes(), err
	}
	if out, stderr, err := run("printf ok > " + shellQuote(filepath.Join(additional, "ok"))); err != nil {
		t.Fatalf("additional write failed: %v stdout=%s stderr=%s", err, out, stderr)
	}
	if out, stderr, err := run("printf ok > " + shellQuote(filepath.Join(scratch, "ok"))); err != nil {
		t.Fatalf("scratch write failed: %v stdout=%s stderr=%s", err, out, stderr)
	}
	if out, stderr, err := run("printf no > " + shellQuote(filepath.Join(blocked, "no"))); err == nil {
		t.Fatalf("disallowed write unexpectedly succeeded: stdout=%s stderr=%s", out, stderr)
	}
	if out, stderr, err := run("cat " + shellQuote(secret)); err == nil || len(out) != 0 || bytes.Contains(stderr, []byte(secretSentinel)) {
		t.Fatalf("outside read unexpectedly succeeded or leaked data: %v stdout=%q stderr=%q", err, out, stderr)
	}
	if profile, err := s.Profile(); err != nil {
		t.Fatal(err)
	} else {
		s.SetAllowNetwork(true)
		profile, err = s.Profile()
		if err != nil || !strings.Contains(profile, "(allow network-outbound)") {
			t.Fatalf("network allow profile missing: %v %s", err, profile)
		}
	}
	testStrictNetwork(t, s)
}

// testStrictNetwork exercises the kernel profile with a local TCP listener on
// a real non-loopback interface. Loopback-only tests are insufficient because
// the profile intentionally permits local service sockets even when outbound
// network access is disabled.
func testStrictNetwork(t *testing.T, s *Sandbox) {
	t.Helper()
	host := nonLoopbackIPv4(t)
	if host == "" {
		t.Skip("no active non-loopback IPv4 interface")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot bind local non-loopback listener on %s: %v", host, err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 4)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_, _ = conn.Write([]byte("connected\n"))
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()

	// Use nc through a shell variable so the userspace command classifier does
	// not decide the outcome. The test is specifically checking the kernel
	// profile's non-loopback network rule; both child invocations enter the
	// same strict profile.
	command := fmt.Sprintf("N=n; /usr/bin/${N}c -w 1 %s %s", shellQuote(host), shellQuote(port))
	run := func() (stdout, stderr []byte, err error) {
		prepared, prepareErr := PrepareCommand(s, command)
		if prepareErr != nil {
			return nil, nil, prepareErr
		}
		child := exec.Command("/bin/sh", "-c", prepared)
		child.Dir = s.Workspace
		var out, diagnostic bytes.Buffer
		child.Stdout = &out
		child.Stderr = &diagnostic
		err = child.Run()
		return out.Bytes(), diagnostic.Bytes(), err
	}

	// The command contains no client-name regex (it uses Python's socket API),
	// so a successful false-mode result would prove the kernel profile itself
	// failed to deny the non-loopback connection.
	s.SetAllowNetwork(false)
	if out, stderr, runErr := run(); runErr == nil || strings.Contains(string(out), "connected") {
		t.Fatalf("network-disabled child connected to %s:%s: %v stdout=%s stderr=%s", host, port, runErr, out, stderr)
	}
	select {
	case <-accepted:
		t.Fatal("network-disabled child reached the non-loopback listener")
	default:
	}

	s.SetAllowNetwork(true)
	out, stderr, runErr := run()
	if runErr != nil || !strings.Contains(string(out), "connected") {
		t.Fatalf("network-enabled child could not connect to %s:%s: %v stdout=%s stderr=%s", host, port, runErr, out, stderr)
	}
	select {
	case <-accepted:
	case <-timeAfterNetworkAccept():
		t.Fatal("network-enabled child did not reach the listener")
	}
	_ = listener.Close()
	<-acceptDone
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Logf("enumerating interfaces: %v", err)
		return ""
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return ""
}

func timeAfterNetworkAccept() <-chan time.Time {
	return time.After(2 * time.Second)
}
