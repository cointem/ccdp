package tui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

func TestModePickerKeepsRevisionAfterCommandCompletion(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		name := "memory"
		if persistent {
			name = "persisted"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			cfg := config.Default()
			cfg.Workspace = root
			cfg.SessionDir = filepath.Join(root, "sessions")
			cfg.NoSessionPersistence = !persistent
			cfg.APIKey = "test-key"
			cfg.BaseURL = "http://127.0.0.1:1"
			ag, err := agent.New(&cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ag.Close()
			m := NewWithClient(ag, root, false)
			defer m.Close()
			sub, err := ag.Watch(context.Background(), protocol.Cursor{})
			if err != nil {
				t.Fatal(err)
			}
			m.subscription = sub
			applyUpdate := func(update protocol.Update) {
				next, _ := m.handleWatchUpdate(watchUpdateMsg{sub: sub, update: update,
					sessionID: protocol.SessionID(m.sessionID), generation: m.watchGeneration})
				m = modelValue(t, next)
			}
			drain := func() {
				for {
					select {
					case update, ok := <-sub.Updates():
						if !ok {
							t.Fatal("watch closed unexpectedly")
						}
						applyUpdate(update)
					default:
						return
					}
				}
			}
			drain()
			for _, line := range []string{"/mode", "/mode", "/mode plan", "/mode default", "/plan on", "/plan off", "/sandbox none", "/sandbox confine", "/clear", "/save", "/mode"} {
				next, cmd := m.runCommand(line)
				m = modelValue(t, next)
				if line == "/mode" {
					if m.picker == nil {
						t.Fatal("/mode did not open a picker")
					}
					next, cmd = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
					m = modelValue(t, next)
				}
				if cmd == nil {
					t.Fatalf("%s returned no command", line)
				}
				msg, ok := cmd().(commandReceiptMsg)
				if !ok || msg.receipt.Rejected() {
					t.Fatalf("%s receipt = %+v", line, msg.receipt)
				}
				m.applyReceipt(msg.receipt, msg.purpose)
				if msg.receipt.Status == protocol.ReceiptScheduled {
					timeout := time.After(3 * time.Second)
					completed := false
					for !completed {
						select {
						case update, ok := <-sub.Updates():
							if !ok {
								t.Fatal("watch closed before command completed")
							}
							applyUpdate(update)
							if r := update.Receipt; r != nil && r.CommandID == msg.receipt.CommandID && r.Status != protocol.ReceiptScheduled {
								if r.Rejected() {
									t.Fatalf("%s completion = %+v", line, r)
								}
								completed = true
							}
						case <-timeout:
							t.Fatalf("%s did not complete", line)
						}
					}
				}
				drain()
				current, err := ag.Snapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if m.snapshot.Revision != current.Revision {
					t.Fatalf("after %s, UI revision = %+v; runtime = %+v", line, m.snapshot.Revision, current.Revision)
				}
			}
		})
	}
}
