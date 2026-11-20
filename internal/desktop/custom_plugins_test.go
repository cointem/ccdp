package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"ccdp/internal/skills"
)

func writePluginFixture(t *testing.T, root, name, text string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pluginFixture(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	writePluginFixture(t, src, ".codex-plugin/plugin.json", `{
		"name":"reviews","version":"1.2","skills":"./skills/","description":"Check documentation",
		"interface":{"displayName":"Review Desk"}
	}`)
	writePluginFixture(t, src, "skills/review/SKILL.md", "---\nname: review-docs\n---\nPlugin skill")
	writePluginFixture(t, src, "skills/review/references/checks.md", "Checklist")
	return src
}

func TestLocalPluginLifecycleAndSkillPrecedence(t *testing.T) {
	user := filepath.Join(t.TempDir(), "skills")
	src := pluginFixture(t)
	if err := installCustomPlugin(user, src); err != nil {
		t.Fatal(err)
	}
	if err := installCustomPlugin(user, src); err == nil {
		t.Fatal("overwrote an installed plugin")
	}
	rows, err := installedCustomPlugins(user)
	if err != nil || len(rows) != 1 || rows[0].ID != "reviews" || rows[0].Name != "Review Desk" ||
		rows[0].Version != "1.2" || !slices.Equal(rows[0].Skills, []string{"review-docs"}) {
		t.Fatalf("installed metadata: %+v, %v", rows, err)
	}
	checkRuntime := func(want string) {
		t.Helper()
		s := skills.NewStore()
		if err := s.LoadChecked(filepath.Abs, user); err != nil {
			t.Fatal(err)
		}
		if sk, ok := s.Get("review-docs"); !ok || sk.Source != "user" || sk.Body != want {
			t.Fatalf("runtime skill = %+v", sk)
		}
	}
	checkRuntime("Plugin skill")
	writePluginFixture(t, user, "ordinary/SKILL.md", "---\nname: review-docs\n---\nOrdinary skill")
	checkRuntime("Ordinary skill")
	installed := filepath.Join(user, skills.ManagedPluginsDir, "reviews")
	resource := writePluginFixture(t, installed, "skills/review/references/checks.md", "My edits")
	if err := uninstallCustomPlugin(user, "reviews"); err == nil {
		t.Fatal("removed a modified package")
	}
	if b, _ := os.ReadFile(resource); string(b) != "My edits" {
		t.Fatal("lost the edited resource")
	}
	if err := os.WriteFile(resource, []byte("Checklist"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := uninstallCustomPlugin(user, "reviews"); err != nil {
		t.Fatal(err)
	}
	checkRuntime("Ordinary skill")
	rows, err = installedCustomPlugins(user)
	if err != nil || len(rows) != 0 {
		t.Fatalf("uninstalled plugin still listed: %+v, %v", rows, err)
	}
}

func TestLocalPluginRejectsUnsupportedAndPartialPackages(t *testing.T) {
	for _, tc := range []struct{ name, target, value string }{
		{"declared apps", ".codex-plugin/plugin.json", `{"name":"reviews","apps":"./.app.json"}`},
		{"MCP config", ".mcp.json", `{}`},
		{"invalid identifier", ".codex-plugin/plugin.json", `{"name":"../outside"}`},
		{"invalid skill", "skills/review/SKILL.md", " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := filepath.Join(t.TempDir(), "skills")
			src := pluginFixture(t)
			writePluginFixture(t, src, tc.target, tc.value)
			if err := installCustomPlugin(user, src); err == nil {
				t.Fatal("invalid package was published")
			}
			rows, err := installedCustomPlugins(user)
			if err != nil || len(rows) != 0 {
				t.Fatalf("partial installation: %+v, %v", rows, err)
			}
		})
	}
}

func TestLocalPluginRejectsLinkedFilesAndUnmanagedDirectories(t *testing.T) {
	user := filepath.Join(t.TempDir(), "skills")
	src := pluginFixture(t)
	foreign := writePluginFixture(t, t.TempDir(), "private.txt", "private")
	if err := os.Symlink(foreign, filepath.Join(src, "skills/review/external")); err != nil {
		t.Fatal(err)
	}
	if err := installCustomPlugin(user, src); err == nil {
		t.Fatal("external link was followed")
	}
	unmanaged := writePluginFixture(t, user, filepath.Join(skills.ManagedPluginsDir, "keep/my-work"), "user file")
	if err := uninstallCustomPlugin(user, "keep"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged package was treated as installed: %v", err)
	}
	if b, err := os.ReadFile(unmanaged); err != nil || string(b) != "user file" {
		t.Fatal("unmanaged data lost")
	}
}
