package fsops

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorizedReadRejectsReplacementAfterPolicyCheck(t *testing.T) {
	for _, replacement := range []string{"parent-symlink", "file-symlink", "hardlink"} {
		t.Run(replacement, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			safe, denied := filepath.Join(root, "safe"), filepath.Join(root, "denied")
			for _, dir := range []string{safe, denied} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			path, secret := filepath.Join(safe, "data"), filepath.Join(denied, "data")
			if err := os.WriteFile(path, []byte("allowed"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			checks := 0
			authorize := func(p string) (string, error) {
				canonical, err := filepath.EvalSymlinks(p)
				if err != nil {
					return "", err
				}
				if strings.HasPrefix(canonical, denied+string(filepath.Separator)) {
					return "", fmt.Errorf("denied")
				}
				checks++
				if checks == 2 {
					switch replacement {
					case "parent-symlink":
						if err := os.Rename(safe, safe+"-old"); err != nil {
							t.Fatal(err)
						}
						err = os.Symlink(denied, safe)
					case "file-symlink":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						err = os.Symlink(secret, path)
					case "hardlink":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						err = os.Link(secret, path)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				return p, nil
			}
			f, err := OpenAuthorizedRegular("", path, authorize)
			if err == nil {
				f.Close()
				t.Fatal("opened replaced path after authorization")
			}
			if checks != 2 {
				t.Fatalf("policy checks=%d", checks)
			}
		})
	}
}

func TestAuthorizedReadResolvesAliasesAndConfinesRoot(t *testing.T) {
	root := t.TempDir()
	path, alias := filepath.Join(root, "data"), filepath.Join(root, "alias")
	if err := os.WriteFile(path, []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	authorize := func(p string) (string, error) { return p, nil }
	f, err := OpenAuthorizedRegular(root, alias, authorize)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != "allowed" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenAuthorizedRegular(root, alias, authorize); err == nil {
		f.Close()
		t.Fatal("root escape accepted")
	}
}
