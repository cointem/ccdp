package sandbox

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSeatbeltProfileIntegration uses only temporary directories and the
// production absolute-path backend invocation.
func TestSeatbeltProfileIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CCDP_SEATBELT_INTEGRATION") != "1" {
		t.Skip("opt-in macOS Seatbelt integration test")
	}
	// Go's TempDir-generated test names can exceed macOS's short UDS path
	// limit. Keep this per-test workspace under /tmp with a short name.
	workspace, err := os.MkdirTemp("/tmp", "ccdp-sb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	outside := t.TempDir()
	additional := filepath.Join(workspace, "additional")
	scratch := filepath.Join(workspace, "scratch")
	blocked := filepath.Join(workspace, "blocked")
	readOnly := filepath.Join(workspace, "read-only")
	for _, dir := range []string{additional, scratch, blocked, readOnly} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(blocked, "hidden"), []byte("denied"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	const secretSentinel = "ccdp-outside-read-sentinel-7f4b"
	if err := os.WriteFile(secret, []byte(secretSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(workspace)
	s.AddDir(additional)
	s.SetScratchDir(scratch)
	s.AddDisallowedDir(blocked)
	s.AddReadOnlyDir(readOnly)
	protectedMissing := filepath.Join(workspace, ".ccdp")
	s.AddProtectedDir(protectedMissing)
	protectedTarget := filepath.Join(outside, "protected-target")
	if err := os.WriteFile(protectedTarget, []byte("control"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectedAlias := filepath.Join(workspace, "protected-alias")
	if err := os.Symlink(protectedTarget, protectedAlias); err != nil {
		t.Fatal(err)
	}
	s.AddProtectedDir(protectedAlias)
	movable := filepath.Join(workspace, "movable")
	deniedNested := filepath.Join(movable, "secret")
	if err := os.MkdirAll(deniedNested, 0o700); err != nil {
		t.Fatal(err)
	}
	s.AddDisallowedDir(deniedNested)

	run := func(command string) (stdout, stderr []byte, err error) {
		profile, err := s.Profile()
		if err != nil {
			return nil, nil, err
		}
		child := exec.Command(BackendPath, "-p", profile, "--", "/bin/sh", "-c", command)
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
	if out, stderr, err := run("printf no > " + shellQuote(filepath.Join(readOnly, "no"))); err == nil {
		t.Fatalf("overlapping read-only directory was writable: stdout=%s stderr=%s", out, stderr)
	}
	if out, stderr, err := run("cat " + shellQuote(secret)); err != nil || !bytes.Contains(out, []byte(secretSentinel)) {
		t.Fatalf("outside read failed: %v stdout=%q stderr=%q", err, out, stderr)
	}
	if out, stderr, err := run("printf no > " + shellQuote(filepath.Join(outside, "no"))); err == nil {
		t.Fatalf("outside write unexpectedly succeeded: stdout=%q stderr=%q", out, stderr)
	}
	if out, stderr, err := run("cat " + shellQuote(filepath.Join(blocked, "hidden"))); err == nil {
		t.Fatalf("explicitly denied read unexpectedly succeeded: stdout=%q stderr=%q", out, stderr)
	}
	if out, stderr, err := run("mkdir " + shellQuote(protectedMissing)); err == nil {
		t.Fatalf("sandbox created a protected path: stdout=%q stderr=%q", out, stderr)
	}
	if out, stderr, err := run("rm " + shellQuote(protectedAlias)); err == nil {
		t.Fatalf("sandbox removed a protected symlink entry: stdout=%q stderr=%q", out, stderr)
	}
	if out, stderr, err := run("mv " + shellQuote(movable) + " " + shellQuote(filepath.Join(workspace, "relocated"))); err == nil {
		t.Fatalf("sandbox relocated an ancestor of a denied path: stdout=%q stderr=%q", out, stderr)
	}
	if out, stderr, err := run("mv " + shellQuote(additional) + " " + shellQuote(filepath.Join(workspace, "renamed-additional"))); err == nil {
		t.Fatalf("sandbox replaced an authorized writable root: stdout=%q stderr=%q", out, stderr)
	}
	testSeatbeltSocketCapabilities(t, s)
}

// testSeatbeltSocketCapabilities verifies that general internet egress and
// localhost services are independent Seatbelt capabilities. It uses TCP and
// Unix socket helpers built in the authorized workspace instead of relying on
// curl/nc availability or a live external network.
func testSeatbeltSocketCapabilities(t *testing.T, s *Sandbox) {
	t.Helper()
	helper := buildSocketHelper(t, s.Workspace)
	allowedAccepted, allowedPort := startLoopbackTCPListener(t)
	otherAccepted, otherPort := startLoopbackTCPListener(t)
	if allowedPort == otherPort {
		t.Fatal("ephemeral listener ports unexpectedly collided")
	}
	allowedAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(allowedPort)))
	otherAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(otherPort)))

	// The default policy rejects loopback. Enabling general internet egress
	// must still reject loopback and must produce scoped TCP/UDP rules rather
	// than a blanket network-outbound permission.
	s.SetAllowNetwork(false)
	if out, stderr, runErr := runSocketHelper(s, helper, "connect", allowedAddress); runErr == nil || strings.Contains(string(out), "CONNECTED") {
		t.Fatalf("default policy unexpectedly connected to loopback %s: %v stdout=%q stderr=%q", allowedAddress, runErr, out, stderr)
	}
	assertNoSocketAccepted(t, allowedAccepted, "default-denied loopback connect")

	s.SetAllowNetwork(true)
	profile, err := s.Profile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(allow network-outbound (require-all (remote tcp) (require-not (remote ip "localhost:*"))))`,
		`(allow network-outbound (require-all (remote udp) (require-not (remote ip "localhost:*"))))`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("internet egress profile missing scoped rule %q: %s", want, profile)
		}
	}
	if strings.Contains(profile, "(allow network-outbound)\n") {
		t.Fatalf("internet egress profile contains a blanket network-outbound rule: %s", profile)
	}
	if out, stderr, runErr := runSocketHelper(s, helper, "connect", allowedAddress); runErr == nil || strings.Contains(string(out), "CONNECTED") {
		t.Fatalf("internet egress unexpectedly connected to loopback %s: %v stdout=%q stderr=%q", allowedAddress, runErr, out, stderr)
	}
	assertNoSocketAccepted(t, allowedAccepted, "internet egress loopback connect")
	if out, stderr, runErr := runSocketHelper(s, helper, "udp", "127.0.0.1:53"); runErr == nil || strings.Contains(string(out), "SENT") {
		t.Fatalf("internet egress unexpectedly enabled local DNS stub: %v stdout=%q stderr=%q", runErr, out, stderr)
	}

	// Turn general egress back off, then authorize exactly one loopback TCP
	// connect port. A second listener on a different port must remain denied.
	s.SetAllowNetwork(false)
	if err := s.AddLocalService(LocalService{Direction: "connect", Protocol: "tcp", Port: allowedPort}); err != nil {
		t.Fatal(err)
	}
	out, connectStderr, runErr := runSocketHelper(s, helper, "connect", allowedAddress)
	if runErr != nil || !strings.Contains(string(out), "CONNECTED") {
		t.Fatalf("explicit connect capability did not reach %s: %v stdout=%q stderr=%q", allowedAddress, runErr, out, connectStderr)
	}
	assertSocketAccepted(t, allowedAccepted, "explicit loopback connect")
	if out, stderr, runErr := runSocketHelper(s, helper, "connect", otherAddress); runErr == nil || strings.Contains(string(out), "CONNECTED") {
		t.Fatalf("connect capability for port %d unexpectedly reached other port %d: %v stdout=%q stderr=%q", allowedPort, otherPort, runErr, out, stderr)
	}
	assertNoSocketAccepted(t, otherAccepted, "connect to unauthorized port")

	// General egress must not enable Unix-domain sockets. This listener lives in
	// the workspace so a failure cannot be attributed to file-path visibility.
	socketPath := filepath.Join(s.Workspace, "u.sock")
	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unixAccepted := make(chan struct{}, 1)
	unixAcceptDone := make(chan struct{})
	go func() {
		defer close(unixAcceptDone)
		conn, acceptErr := unixListener.Accept()
		if acceptErr == nil {
			unixAccepted <- struct{}{}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = unixListener.Close()
		<-unixAcceptDone
	})
	s.SetAllowNetwork(true)
	if out, stderr, runErr := runSocketHelper(s, helper, "unix", socketPath); runErr == nil || strings.Contains(string(out), "CONNECTED") {
		t.Fatalf("internet egress unexpectedly enabled Unix-domain connect: %v stdout=%q stderr=%q", runErr, out, stderr)
	}
	assertNoSocketAccepted(t, unixAccepted, "Unix-domain connect with internet egress")

	// External egress and a TCP connect grant do not imply the ability to bind
	// or accept on localhost. The listen grant is scoped to one concrete port.
	listenPort := unusedLoopbackTCPPort(t)
	otherListenPort := unusedLoopbackTCPPort(t)
	for otherListenPort == listenPort {
		otherListenPort = unusedLoopbackTCPPort(t)
	}
	listenAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(listenPort)))
	otherListenAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(otherListenPort)))
	if out, stderr, runErr := runSocketHelper(s, helper, "bind", listenAddress); runErr == nil || strings.Contains(string(out), "BOUND") {
		t.Fatalf("internet egress unexpectedly enabled localhost bind %s: %v stdout=%q stderr=%q", listenAddress, runErr, out, stderr)
	}
	if out, stderr, runErr := runSocketHelper(s, helper, "bind", otherListenAddress); runErr == nil || strings.Contains(string(out), "BOUND") {
		t.Fatalf("internet egress unexpectedly enabled localhost bind %s: %v stdout=%q stderr=%q", otherListenAddress, runErr, out, stderr)
	}
	if err := s.AddLocalService(LocalService{Direction: "listen", Protocol: "tcp", Port: listenPort}); err != nil {
		t.Fatal(err)
	}
	child, stdout, serveStderr, err := startSocketHelper(s, helper, "serve", listenAddress)
	if err != nil {
		t.Fatalf("listen capability did not bind %s: %v stderr=%q", listenAddress, err, serveStderr.String())
	}
	if err := verifySocketHelperReady(child, stdout, serveStderr); err != nil {
		t.Fatalf("listen helper did not become ready on %s: %v stderr=%q", listenAddress, err, serveStderr.String())
	}
	conn, err := net.DialTimeout("tcp4", listenAddress, time.Second)
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("listen capability did not accept localhost inbound connection at %s: %v", listenAddress, err)
	}
	_ = conn.Close()
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case waitErr := <-done:
		if waitErr != nil {
			t.Fatalf("listen helper exited after inbound connection: %v stderr=%q", waitErr, serveStderr.String())
		}
	case <-time.After(2 * time.Second):
		_ = child.Process.Kill()
		_ = <-done
		t.Fatalf("listen helper did not accept inbound connection on %s", listenAddress)
	}
	if out, stderr, runErr := runSocketHelper(s, helper, "bind", otherListenAddress); runErr == nil || strings.Contains(string(out), "BOUND") {
		t.Fatalf("listen capability for port %d unexpectedly bound other port %d: %v stdout=%q stderr=%q", listenPort, otherListenPort, runErr, out, stderr)
	}

	// A real external-egress success check depends on the machine having a
	// reachable public endpoint. Keep it separate and explicit so local socket
	// checks never use a LAN listener as a proxy for internet access.
	t.Run("external_egress", func(t *testing.T) {
		target := strings.TrimSpace(os.Getenv("CCDP_SEATBELT_EXTERNAL_TARGET"))
		if target == "" {
			t.Skip("set CCDP_SEATBELT_EXTERNAL_TARGET to a reachable public IPv4 address and port")
		}
		host, _, splitErr := net.SplitHostPort(target)
		if splitErr != nil {
			t.Fatalf("CCDP_SEATBELT_EXTERNAL_TARGET must be an IPv4 address and port: %v", splitErr)
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			t.Fatalf("CCDP_SEATBELT_EXTERNAL_TARGET must be a public IPv4 address, got %q", host)
		}
		s.SetAllowNetwork(true)
		out, stderr, connectErr := runSocketHelper(s, helper, "connect", target)
		if connectErr != nil || !strings.Contains(string(out), "CONNECTED") {
			t.Fatalf("internet egress did not reach %s: %v stdout=%q stderr=%q", target, connectErr, out, stderr)
		}
	})
}

func buildSocketHelper(t *testing.T, workspace string) string {
	t.Helper()
	source := filepath.Join(workspace, "socket_helper.go")
	binary := filepath.Join(workspace, "socket-helper")
	if err := os.WriteFile(source, []byte(socketHelperSource), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(workspace, "go-cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("Go is required to build the Seatbelt socket helper: %v", err)
	}
	build := exec.Command(goPath, "build", "-o", binary, source)
	build.Dir = workspace
	build.Env = testEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GO111MODULE": "off",
		"GOCACHE":     cache,
		"GOENV":       "off",
		"GOPROXY":     "off",
		"GOTOOLCHAIN": "local",
	})
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Seatbelt socket helper: %v: %s", err, output)
	}
	return binary
}

const socketHelperSource = `package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

func main() {
	if len(os.Args) != 3 {
		fail(fmt.Errorf("usage: socket-helper {connect|udp|unix|bind|serve} address"))
	}
	switch os.Args[1] {
	case "connect":
		conn, err := net.DialTimeout("tcp4", os.Args[2], time.Second)
		if err != nil { fail(err) }
		fmt.Println("CONNECTED")
		_ = conn.Close()
	case "udp":
		conn, err := net.DialTimeout("udp4", os.Args[2], time.Second)
		if err != nil { fail(err) }
		if _, err := conn.Write([]byte("x")); err != nil { fail(err) }
		fmt.Println("SENT")
		_ = conn.Close()
	case "unix":
		conn, err := net.DialTimeout("unix", os.Args[2], time.Second)
		if err != nil { fail(err) }
		fmt.Println("CONNECTED")
		_ = conn.Close()
	case "bind":
		listener, err := net.Listen("tcp4", os.Args[2])
		if err != nil { fail(err) }
		fmt.Println("BOUND")
		_ = listener.Close()
	case "serve":
		listener, err := net.Listen("tcp4", os.Args[2])
		if err != nil { fail(err) }
		fmt.Println("READY")
		conn, err := listener.Accept()
		if err != nil { fail(err) }
		fmt.Println("ACCEPTED")
		_ = conn.Close()
		_ = listener.Close()
	default:
		fail(fmt.Errorf("unknown mode %q", os.Args[1]))
	}
}
`

func runSocketHelper(s *Sandbox, helper string, args ...string) ([]byte, []byte, error) {
	profile, err := s.Profile()
	if err != nil {
		return nil, nil, err
	}
	argv := append([]string{BackendPath, "-p", profile, "--", helper}, args...)
	child := exec.Command(argv[0], argv[1:]...)
	child.Dir = s.Workspace
	var out, diagnostic bytes.Buffer
	child.Stdout = &out
	child.Stderr = &diagnostic
	err = child.Run()
	return out.Bytes(), diagnostic.Bytes(), err
}

func startSocketHelper(s *Sandbox, helper string, args ...string) (*exec.Cmd, *bufio.Reader, *bytes.Buffer, error) {
	profile, err := s.Profile()
	if err != nil {
		return nil, nil, nil, err
	}
	argv := append([]string{BackendPath, "-p", profile, "--", helper}, args...)
	child := exec.Command(argv[0], argv[1:]...)
	child.Dir = s.Workspace
	stdout, err := child.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		return nil, nil, nil, err
	}
	return child, bufio.NewReader(stdout), &stderr, nil
}

func verifySocketHelperReady(child *exec.Cmd, stdout *bufio.Reader, stderr *bytes.Buffer) error {
	ready := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := stdout.ReadString('\n')
		ready <- struct {
			line string
			err  error
		}{line: line, err: err}
	}()
	select {
	case result := <-ready:
		if result.err != nil || result.line != "READY\n" {
			_ = child.Wait()
			return fmt.Errorf("got readiness line %q: %v (stderr %q)", result.line, result.err, stderr.String())
		}
		return nil
	case <-time.After(2 * time.Second):
		_ = child.Process.Kill()
		_ = child.Wait()
		return fmt.Errorf("timed out waiting for READY (stderr %q)", stderr.String())
	}
}

func startLoopbackTCPListener(t *testing.T) (<-chan struct{}, uint16) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	accepted := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return accepted, port
}

func assertSocketAccepted(t *testing.T, accepted <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatalf("listener did not observe %s", operation)
	}
}

func assertNoSocketAccepted(t *testing.T, accepted <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-accepted:
		t.Fatalf("listener unexpectedly observed %s", operation)
	case <-time.After(150 * time.Millisecond):
	}
}

func unusedLoopbackTCPPort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func testEnvironment(entries []string, values map[string]string) []string {
	out := make([]string, 0, len(entries)+len(values))
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := values[key]; !replace {
			out = append(out, entry)
		}
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
