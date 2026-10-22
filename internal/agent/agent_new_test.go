package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/config"
)

// newAgentForTest builds an agent with a temp session dir for unit tests that
// do not need the LLM.
func newAgentForTest(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = filepath.Join(dir, "sessions")

	ag, err := testNew(t, &cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestImageContentParts(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()

	// Write a tiny PNG (1x1 transparent) into the workspace.
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
		0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(filepath.Join(ag.services.cfg.Workspace, "pic.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}

	text := "Look at ![diagram](pic.png) and explain."
	parts, _, hasImage, err := imageContentPartsAtBudget(ag.services.cfg.Workspace, text, maxAttachmentBytes, ag.currentSandbox().ResolveRead)
	if err != nil || !hasImage {
		t.Fatalf("expand image: hasImage=%v err=%v", hasImage, err)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts (text + image + text), got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "Look at " {
		t.Errorf("bad first text part: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("expected image part: %+v", parts[1])
	}
	if len(parts[1].ImageURL.URL) < 30 || parts[1].ImageURL.URL[:22] != "data:image/png;base64," {
		t.Errorf("bad data URL: %.30s…", parts[1].ImageURL.URL)
	}
	if parts[2].Type != "text" || parts[2].Text != " and explain." {
		t.Errorf("bad trailing text part: %+v", parts[2])
	}

	// Missing file remains plain text and cannot attach an image.
	if _, _, found, err := imageContentPartsAtBudget(ag.services.cfg.Workspace, "![x](missing.png) text", maxAttachmentBytes, ag.currentSandbox().ResolveRead); found || err == nil {
		t.Errorf("expected missing image error, got found=%v err=%v", found, err)
	}
	// No markers → nil.
	if parts, _, found, err := imageContentPartsAtBudget(ag.services.cfg.Workspace, "plain text", maxAttachmentBytes, ag.currentSandbox().ResolveRead); parts != nil || found || err != nil {
		t.Errorf("expected no expansion for plain text, got parts=%+v found=%v err=%v", parts, found, err)
	}
}

func TestTraceWritten(t *testing.T) {
	ag := newAgentForTest(t)
	ag.emit(Event{Type: EventUserMsg, Text: "hello trace"})
	ag.Close()

	data, err := os.ReadFile(ag.TracePath())
	if err != nil {
		t.Fatalf("trace file missing: %v", err)
	}
	if !strings.Contains(string(data), "hello trace") {
		t.Errorf("trace does not contain event: %s", data)
	}
}
