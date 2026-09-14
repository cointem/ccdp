package mcp

import (
	"ccdp/internal/tools"
	"sync"
	"testing"
)

func TestRetainedLeasePinsExactGenerationAfterReload(t *testing.T) {
	m := NewManager()
	defer m.Close()
	reg := tools.NewRegistry()
	reg.Register(tools.NewReadTool())
	step := m.AcquireStep(reg)
	m.mu.Lock()
	original := m.generation
	m.generation++
	m.mu.Unlock()
	child := step.Retain()
	if child == nil {
		t.Fatal("cannot retain live step")
	}
	step.Close()
	m.mu.Lock()
	refs := m.refs[original]
	m.mu.Unlock()
	if refs != 1 {
		t.Fatalf("old generation prematurely released: %d", refs)
	}
	if _, ok := child.Tools.Get("Read"); !ok {
		t.Fatal("lost frozen tools")
	}
	child.Close()
	child.Close()
	m.mu.Lock()
	refs = m.refs[original]
	m.mu.Unlock()
	if refs != 0 {
		t.Fatalf("retained generation leaked: %d", refs)
	}
	if step.Retain() != nil {
		t.Fatal("closed lease resurrected")
	}
}

func TestRetainConcurrentWithCloseDoesNotLeak(t *testing.T) {
	m := NewManager()
	defer m.Close()
	for i := 0; i < 100; i++ {
		lease := m.AcquireStep(tools.NewRegistry())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); copy := lease.Retain(); copy.Close() }()
		go func() { defer wg.Done(); lease.Close() }()
		wg.Wait()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.refs) != 0 {
		t.Fatalf("generation refs leaked: %v", m.refs)
	}
}
