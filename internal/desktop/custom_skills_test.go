package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func skillFixture(t *testing.T, dir, name, manifest string) string {
	t.Helper()
	dir = filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCustomSkillInstallScopesAndSupportingFiles(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".ccdp", "config.json")
	source := skillFixture(t, t.TempDir(), "review", "---\ndescription: User review\n---\nRead the changes.")
	if err := os.Mkdir(filepath.Join(source, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "scripts", "check"), []byte("exit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := installLocalSkill(source, "", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ccdp", "skills", "review", "scripts", "check")
	if info, err := os.Stat(path); err != nil || info.Mode()&0100 == 0 {
		t.Fatalf("executable resource lost: %v", err)
	}
	check := func(root, scope, description string) {
		t.Helper()
		listing := loadCustomizationExtensions(root, cfgPath)
		rows := listing.skills
		err := listing.problem + listing.skillsProblem
		if err != "" || len(rows) != 1 {
			t.Fatalf("load %q: %v %v", root, rows, err)
		}
		if rows[0].sourceLabel() != scope || rows[0].detail != description || rows[0].name != "review" {
			t.Fatalf("unexpected selection: %+v", rows)
		}
	}
	check("", "用户", "User review")
	local := skillFixture(t, t.TempDir(), "local-dir", "---\nname: review\ndescription: Project review\n---\nRun checks.")
	if err := installLocalSkill(local, workspace, true); err != nil {
		t.Fatal(err)
	}
	listing := loadCustomizationExtensions(workspace, cfgPath)
	rows := listing.skills
	err := listing.problem + listing.skillsProblem
	if err != "" || len(rows) != 2 || rows[0].name != "review" || rows[1].name != "review" ||
		rows[0].sourceLabel() != "用户" || rows[0].detail != "User review" ||
		rows[1].sourceLabel() != "项目" || rows[1].detail != "Project review" {
		t.Fatalf("expected both installations of review: %+v %v", rows, err)
	}
	check("", "用户", "User review")
	if err := installLocalSkill(local, "", false); err == nil {
		t.Fatal("duplicate effective name accepted in same scope")
	}
	if err := installLocalSkill(source, "", false); err == nil {
		t.Fatal("duplicate directory accepted")
	}
	check("", "用户", "User review")
}

func TestCustomSkillListingKeepsPersonalSkillsWhenProjectIsUnavailable(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(root, ".ccdp")
	if err := os.Mkdir(settings, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings, "settings.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(home, ".ccdp", "skills")
	skillFixture(t, userDir, "personal", "Only in the personal library.")
	cfgPath := filepath.Join(home, ".ccdp", "config.json")
	for _, path := range []string{filepath.Join(root, "moved-away"), root} {
		listing := loadCustomizationExtensions(path, cfgPath)
		rows := listing.skills
		err := listing.problem + listing.skillsProblem
		if err == "" || len(rows) != 1 || rows[0].name != "personal" || rows[0].sourceLabel() != "用户" {
			t.Fatalf("loading %s should return personal skills and a project error: %+v %v", path, rows, err)
		}
	}
}

func TestCustomSkillInvalidInstallLeavesNoPackage(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	source := skillFixture(t, t.TempDir(), "new-skill", "---\nname: new-skill\n---\n")
	if err := installLocalSkill(source, workspace, true); err == nil {
		t.Fatal("runtime would skip empty skill")
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("Fresh checks."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, filepath.Join(source, "resources")); err != nil {
		t.Fatal(err)
	}
	if err := installLocalSkill(source, workspace, true); err == nil {
		t.Fatal("installed package with a linked resource")
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".ccdp", "skills"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("partially installed or stale staging files: %v %v", entries, err)
	}
	other := t.TempDir()
	if err := os.Symlink(home, filepath.Join(other, ".ccdp")); err != nil {
		t.Fatal(err)
	}
	if err := installLocalSkill(source, other, true); err == nil {
		t.Fatal("install escaped selection through .ccdp symlink")
	}
	if _, err := os.Stat(filepath.Join(home, "skills")); !os.IsNotExist(err) {
		t.Fatal("created an install directory outside the selected project")
	}
}
