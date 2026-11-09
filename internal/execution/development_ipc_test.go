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

func TestPreparedDevelopmentIPC(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	workspace, err := os.MkdirTemp("/tmp", "ccdp-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(workspace) })
	c := NewController(t.TempDir() + "/runtime")
	t.Cleanup(func() { c.Close() })
	environment, err := NewEnvironment(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(workspace)
	policy.ExecutorID, policy.LocalDevelopmentNetwork = c.ID, true
	policy.SetScratchDir(filepath.Join(environment, "scratch"))
	if err := os.WriteFile(filepath.Join(workspace, "ipc.py"), []byte(developmentIPCScript), 0600); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"unix-workspace", "unix-temp", "unix-datagram", "socketpair", "udp4", "udp6", "pool", "pty", "files"} {
		t.Run(feature, func(t *testing.T) {
			current := policy.Snapshot()
			current.LocalDevelopmentNetwork = !strings.HasPrefix(feature, "unix-")
			result, err := Run(Request{Context: context.Background(), Command: "python3 ipc.py " + feature, Dir: workspace, Sandbox: current, Timeout: 20 * time.Second})
			if err != nil || result.ExitCode != 0 || result.TimedOut {
				t.Fatalf("%s: %+v %v", feature, result, err)
			}
		})
	}
	for _, command := range []string{"/usr/sbin/sysctl -n hw.ncpu hw.memsize", "/usr/sbin/lsof -p $$"} {
		t.Run(command, func(t *testing.T) {
			result, err := Run(Request{Context: context.Background(), Command: command, Dir: workspace, Sandbox: policy, Timeout: 10 * time.Second})
			if err != nil || result.ExitCode != 0 || result.TimedOut {
				t.Fatalf("%s: %+v %v", command, result, err)
			}
		})
	}
}

const developmentIPCScript = `import fcntl, mmap, multiprocessing, os, pty, socket, sys, tempfile

def square(value):
    return value * value

if __name__ == '__main__':
    feature = sys.argv[1]
    assert os.environ['XDG_RUNTIME_DIR'] == tempfile.gettempdir()
    if feature.startswith('unix-'):
        root = os.getcwd() if feature == 'unix-workspace' else tempfile.mkdtemp(prefix='ipc-')
        path = os.path.join(root, 'test.sock')
        assert len(os.fsencode(path)) < 104, path
        kind = socket.SOCK_DGRAM if feature == 'unix-datagram' else socket.SOCK_STREAM
        with socket.socket(socket.AF_UNIX, kind) as server, socket.socket(socket.AF_UNIX, kind) as client:
            server.settimeout(2)
            server.bind(path)
            if kind == socket.SOCK_STREAM:
                server.listen(1)
            client.connect(path)
            client.sendall(b'ok')
            if kind == socket.SOCK_STREAM:
                conn, _ = server.accept()
                with conn:
                    assert conn.recv(2) == b'ok'
            else:
                assert server.recv(2) == b'ok'
        os.unlink(path)
    elif feature == 'socketpair':
        a, b = socket.socketpair()
        with a, b:
            a.sendall(b'ok')
            assert b.recv(2) == b'ok'
    elif feature.startswith('udp'):
        family, host = (socket.AF_INET, '127.0.0.1') if feature == 'udp4' else (socket.AF_INET6, '::1')
        with socket.socket(family, socket.SOCK_DGRAM) as server, socket.socket(family, socket.SOCK_DGRAM) as client:
            server.settimeout(2)
            server.bind((host, 0))
            client.sendto(b'ok', server.getsockname())
            assert server.recv(2) == b'ok'
    elif feature == 'pool':
        with multiprocessing.get_context('spawn').Pool(2) as pool:
            assert pool.map(square, [2, 3]) == [4, 9]
    elif feature == 'pty':
        master, slave = pty.openpty()
        try:
            os.write(slave, b'ok')
            assert os.read(master, 2) == b'ok'
        finally:
            os.close(master)
            os.close(slave)
    elif feature == 'files':
        with tempfile.TemporaryFile() as file:
            fcntl.flock(file, fcntl.LOCK_EX)
            file.write(b'ok')
            file.flush()
            with mmap.mmap(file.fileno(), 2) as shared:
                assert shared[:] == b'ok'
            fcntl.flock(file, fcntl.LOCK_UN)
    print(feature + ': ok')
`
