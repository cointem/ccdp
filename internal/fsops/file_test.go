package fsops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestIndependentProcessesCannotReplaceSameVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	type child struct {
		cmd *exec.Cmd
		in  io.WriteCloser
		out *bufio.Reader
	}
	var children []child
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestVersionWriterHelper$")
		cmd.Env = append(os.Environ(), "CCDP_VERSION_TEST_PATH="+path)
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		reader := bufio.NewReader(out)
		if ready, err := reader.ReadString('\n'); err != nil || ready != "ready\n" {
			t.Fatal(ready, err)
		}
		children = append(children, child{cmd, in, reader})
	}
	for _, p := range children {
		if _, err := io.WriteString(p.in, "go\n"); err != nil {
			t.Fatal(err)
		}
		_ = p.in.Close()
	}
	wins := 0
	for _, p := range children {
		line, err := p.out.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "written\n" {
			wins++
		} else if line != "conflict\n" {
			t.Fatal(line)
		}
		if err = p.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d writers published the same observed version", wins)
	}
}

func TestVersionWriterHelper(t *testing.T) {
	path := os.Getenv("CCDP_VERSION_TEST_PATH")
	if path == "" {
		return
	}
	v, err := Observe(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	if _, err = bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	lock, err := LockPath(path)
	if err != nil {
		fmt.Println("conflict")
		return
	}
	defer lock.Close()
	if _, err = publishTestFile(context.Background(), filepath.Dir(path), path, []byte("new"), 0600, &v); err != nil {
		fmt.Println("conflict")
	} else {
		fmt.Println("written")
	}
}

func TestVersionDetectsRewriteWithRestoredMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(p, []byte("old"), 0600)
	v, err := Observe(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(p, v.Mtime, v.Mtime); err != nil {
		t.Fatal(err)
	}
	next, err := Observe(p)
	if err != nil {
		t.Fatal(err)
	}
	if next == v {
		t.Fatal("same-size same-mtime edit not detected")
	}
}

func TestWorkspaceLeaseAllowsFileMutation(t *testing.T) {
	dir := t.TempDir()
	lease, err := LeaseWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	path := filepath.Join(dir, "nested", "new")
	lock, err := LockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	v, err := publishTestFile(context.Background(), dir, path, []byte("created"), 0644, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishTestFile(context.Background(), dir, path, []byte("updated"), 0644, &v); err != nil {
		t.Fatal("own create invalidated version", err)
	}
}

func TestOpenRegularRejectsFIFOAndEscapingLink(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegular(dir, fifo); err == nil {
		f.Close()
		t.Fatal("FIFO accepted")
	}
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	_ = os.Symlink(outside, filepath.Join(dir, "link"))
	if f, err := OpenRegular(dir, filepath.Join(dir, "link", "secret")); err == nil {
		f.Close()
		t.Fatal("root escape accepted")
	}
}

func TestReadRejectsLargeContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(p, []byte(strings.Repeat("x", 100)), 0600)
	if _, _, err := Read(p, 10); err == nil {
		t.Fatal("limit ignored")
	}
}

func TestPublishCancellationAfterVersionCheck(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = publishEntry(ctx, r, "file", "regular", 0600, []byte("changed"), false, func() error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("published after cancellation: %q", data)
	}
}

func publishTestFile(ctx context.Context, rootPath, path string, data []byte, mode os.FileMode, expected *Version) (Version, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return Version{}, err
	}
	defer root.Close()
	relative, err := filepath.Rel(rootPath, path)
	if err != nil {
		return Version{}, err
	}
	return PublishAt(ctx, root, relative, data, mode, expected)
}
