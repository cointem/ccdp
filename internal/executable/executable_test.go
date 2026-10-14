package executable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesCommandDirForRelativePATH(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "bin", "cat")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	path, absolute := Resolve("cat", "bin:/usr/bin:/bin", root)
	if path != File(script) || absolute {
		t.Fatalf("resolved %q absolute=%v", path, absolute)
	}
	path, absolute = Resolve("cat", root+"/bin:/usr/bin:/bin", root)
	if path != File(script) || !absolute || Trusted(path) {
		t.Fatalf("project executable trusted: %q %v", path, absolute)
	}
	trusted := FindTrusted("cat", root+"/bin:/usr/bin:/bin")
	if trusted == File(script) || !Trusted(trusted) {
		t.Fatalf("native query selected %q", trusted)
	}
}

func TestTrustUsesCanonicalSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "cat")
	if err := os.Symlink("/bin/cat", link); err != nil {
		t.Fatal(err)
	}
	path := File(link)
	if !Trusted(path) {
		t.Fatalf("system target untrusted: %q", path)
	}
	if Trusted(root) {
		t.Fatal("project root trusted")
	}
}
