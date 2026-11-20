package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigEditorCreatesPrivateFileOnlyAfterValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ccdp", "config.json")
	snapshot, err := readConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"null", "{", `{"providers": {"test": {"models": []}}}`, `{"base_url":"https://example.com"}`} {
		if _, err := saveConfigFile(path, snapshot, invalid); err == nil {
			t.Fatalf("invalid config accepted: %q", invalid)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created a file before a valid save: %v", err)
	}
	if _, err := saveConfigFile(path, snapshot, snapshot.Text); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("new config is readable outside its owner: %v", info.Mode())
	}
}

func TestConfigEditorPreservesSymlinkAndRejectsStaleDraft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	target := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(target, []byte("{}"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("settings.json", path); err != nil {
		t.Fatal(err)
	}
	start, err := readConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := "{\n  \"future_field\": \"keep this exactly\"\n}\n"
	if _, err := saveConfigFile(path, start, edited); err != nil {
		t.Fatal(err)
	}
	if _, err := saveConfigFile(path, start, "{}"); err == nil {
		t.Fatal("stale draft overwrote more recent content")
	}
	if got, err := os.Readlink(path); got != "settings.json" || err != nil {
		t.Fatalf("link target changed: %q, %v", got, err)
	}
	current, err := readConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if current.Text != edited || current.mode != start.mode {
		t.Fatalf("formatting, unknown fields or mode lost on save")
	}
}
