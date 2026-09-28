package tools

import (
	"context"
	"os"
	"testing"
)

func TestRegressionProcessSplitUTF8(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "process-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := &managedProcess{log: f, logPath: f.Name(), done: make(chan struct{}), changed: make(chan struct{})}
	b := []byte("中")
	p.appendOutput(b[:1])
	out, err := p.view(&Context{Context: context.Background()}, 1, 0, 100, 0)
	first := decodeProcess(t, out, err)
	p.appendOutput(b[1:])
	out, err = p.view(&Context{Context: context.Background()}, 1, first.Next, 100, 0)
	next := decodeProcess(t, out, err)
	if first.Output+next.Output != "中" {
		t.Fatalf("split rune corrupted: first=%+v next=%+v", first, next)
	}
}

func TestProcessIncompleteUTF8AtPermanentLogEnd(t *testing.T) {
	for _, capped := range []bool{false, true} {
		t.Run(map[bool]string{false: "exited", true: "capped"}[capped], func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "process-*.log")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			p := &managedProcess{log: f, logPath: f.Name(), done: make(chan struct{}), changed: make(chan struct{})}
			offset := 0
			if capped {
				offset = maxProcessLog - 1
				if _, err := f.Seek(int64(offset), 0); err != nil {
					t.Fatal(err)
				}
				p.logSize, p.total = int64(offset), int64(offset)
			}
			p.appendOutput([]byte{0xe4})
			p.exited = !capped
			out, err := p.view(&Context{Context: context.Background()}, 1, offset, 100, 0)
			view := decodeProcess(t, out, err)
			if view.Next != offset+1 || view.Output != "�" {
				t.Fatalf("permanent incomplete byte stalled: %+v", view)
			}
		})
	}
}
