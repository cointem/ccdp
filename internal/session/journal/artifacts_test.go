package journal

import (
	"context"
	"errors"
	"testing"

	"ccdp/internal/session"
)

func TestMemoryArtifactsOwnBytesAndValidateReads(t *testing.T) {
	store := NewMemoryArtifacts(16)
	data := []byte("payload")
	ref, err := store.PutContext(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	got, err := store.Read(ref, 16)
	if err != nil || string(got) != "payload" {
		t.Fatalf("read=%q err=%v", got, err)
	}
	got[0] = 'X'
	again, err := store.Read(ref, 16)
	if err != nil || string(again) != "payload" {
		t.Fatalf("aliased read=%q err=%v", again, err)
	}
	bad := ref
	bad.Size++
	if _, err := store.Read(bad, 16); !errors.Is(err, ErrBlobCorrupt) {
		t.Fatalf("changed size: %v", err)
	}
	if _, err := store.Read(ref, 2); !errors.Is(err, session.ErrTooLarge) {
		t.Fatalf("read bound: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.PutContext(ctx, []byte("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
}
