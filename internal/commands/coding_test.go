package commands

import "testing"

func TestCodingCommandsPreserveQuotedSelections(t *testing.T) {
	args, e := ParseLine(`/deliver prepare "fix selected behavior" --path "dir with spaces/a.go" --local`)
	if e != nil {
		t.Fatal(e)
	}
	w, e := ParseCodingWorkflow("deliver", args[1:])
	if e != nil {
		t.Fatal(e)
	}
	if w.Message != "fix selected behavior" || len(w.Paths) != 1 || w.Paths[0] != "dir with spaces/a.go" || w.Mode != "commit" {
		t.Fatalf("bad selection: %+v", w)
	}
	w, e = ParseCodingWorkflow("rewind", []string{"prepare", "checkpoint-1", "--mode", "both", "--resolve", "a.go=target"})
	if e != nil || w.Resolutions["a.go"] != "target" {
		t.Fatalf("bad resolution: %+v %v", w, e)
	}
	if _, e = ParseCodingWorkflow("review", []string{"--local"}); e == nil {
		t.Fatal("review accepted delivery-only option")
	}
	if _, e = ParseCodingWorkflow("rewind", []string{"prepare", "checkpoint-1", "--scope", "typo"}); e == nil {
		t.Fatal("silently accepted unknown recovery scope")
	}
}
