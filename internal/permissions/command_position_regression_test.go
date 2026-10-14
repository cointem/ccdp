package permissions

import (
	"ccdp/internal/sandbox"
	"testing"
)

func TestDangerousWordsAreDataOutsideExecutablePositions(t *testing.T) {
	manager := NewManager(ModeBypass, Policy{})
	for _, command := range []string{"grep -n shutdown main.go", "git log --grep=shutdown", "git log --grep='git reset --hard'", "cat reboot.go", "echo mkfs", "rm shutdown.txt", "echo 'git reset --hard'", "sh -c 'echo reboot'"} {
		if denied, reason := manager.HardDeny("Bash", map[string]any{"command": command}); denied {
			t.Errorf("%s: %s", command, reason)
		}
	}
	for _, command := range []string{"shutdown -h now", "/sbin/reboot", "env /sbin/shutdown now", "sh -c 'reboot'", "git reset --hard", "git push origin -f", "rm -rf /", "dd if=input of=/dev/disk2", "mkfs.ext4 /dev/disk2"} {
		if denied, _ := manager.HardDeny("Bash", map[string]any{"command": command}); !denied {
			t.Errorf("dangerous executable allowed: %s", command)
		}
	}
	explicit := NewManager(ModeBypass, Policy{AlwaysDeny: []string{"Bash:*shutdown*"}})
	if denied, _ := explicit.HardDeny("Bash", map[string]any{"command": "grep shutdown file"}); !denied {
		t.Fatal("explicit user deny rule changed")
	}
}

func TestInteractiveNamesAreDataOutsideExecutablePositions(t *testing.T) {
	for _, command := range []string{"git show HEAD:top.go", "rg --no-config vim .", "cat src/less.go", "echo top", "sh -c 'echo vim'"} {
		if err := sandbox.CheckInteractive(command); err != nil {
			t.Errorf("%s: %v", command, err)
		}
	}
	for _, argv := range [][]string{{"cat", "src/less.go"}, {"git", "show", "HEAD:top.go"}, {"rg", "--no-config", "vim", "."}} {
		if err := sandbox.CheckInteractiveArgv(argv); err != nil {
			t.Errorf("%v: %v", argv, err)
		}
	}
	for _, command := range []string{"vim file", "env vim file", "sh -c 'vim file'", "git add -p", "git rebase --interactive", "git commit --amend -i", "ssh -tt host"} {
		if err := sandbox.CheckInteractive(command); err == nil {
			t.Errorf("interactive program allowed: %s", command)
		}
	}
}
