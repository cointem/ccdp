package commands

import "testing"

func TestDefaultCatalogHasStableCommandSurface(t *testing.T) {
	catalog := Default()
	names := catalog.Names()
	if len(names) != 55 {
		t.Fatalf("default command surface has %d spellings, want 55", len(names))
	}
	if canonical, ok := catalog.Canonical("/exit"); !ok || canonical != "quit" {
		t.Fatalf("exit alias resolved to %q, %v; want quit", canonical, ok)
	}
	if entry, ok := catalog.Lookup("/config"); !ok || entry.Name != "config" {
		t.Fatalf("config lookup failed: %#v, %v", entry, ok)
	}
	if catalog.ShouldTranscript("config", []string{"set", "model", "x"}) {
		t.Fatal("config set should be transient")
	}
	if !catalog.ShouldTranscript("config", nil) {
		t.Fatal("plain config should produce a report")
	}
	if entry, ok := catalog.Lookup("/details"); !ok || entry.Name != "transcript" || entry.Busy != BusyAllow || entry.Transcript || entry.Mutation {
		t.Fatalf("details should be a read-only, transient transcript reader: %#v", entry)
	}
	if got := catalog.Suggestions("pr-"); len(got) != 1 || got[0] != "pr-comments" {
		t.Fatalf("suggestions = %#v, want pr-comments", got)
	}
}

func TestCatalogOwnsEntrySlices(t *testing.T) {
	aliases := []string{"old"}
	transient := []string{"set"}
	catalog := New([]Entry{{Name: "config", Aliases: aliases, TransientSubcommands: transient}})
	aliases[0] = "mutated"
	transient[0] = "mutated"
	entry, ok := catalog.Lookup("config")
	if !ok || len(entry.Aliases) != 1 || entry.Aliases[0] != "old" || entry.TransientSubcommands[0] != "set" {
		t.Fatalf("catalog entry changed through caller slices: %#v", entry)
	}
	entry.Aliases[0] = "changed"
	entry.TransientSubcommands[0] = "changed"
	again, _ := catalog.Lookup("config")
	if again.Aliases[0] != "old" || again.TransientSubcommands[0] != "set" {
		t.Fatalf("lookup did not return a copy: %#v", again)
	}
}
