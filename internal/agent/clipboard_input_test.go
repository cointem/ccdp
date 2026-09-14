package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func inputPNG(t *testing.T, red uint8) protocol.InputImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: red, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return protocol.InputImage{MediaType: "image/png", Data: b.Bytes()}
}

func TestClipboardInputReachesProviderAsImage(t *testing.T) {
	provider := &formalCompletionProvider{}
	a, _ := formalCompletionAgent(t, provider)
	img := inputPNG(t, 255)
	cmd := protocol.NewSubmitInput("clipboard-send", protocol.SessionID(a.SessionID()), "clipboard-input", "", protocol.InputSteer)
	cmd.Input.Images = []protocol.InputImage{img}
	if receipt, err := a.Submit(context.Background(), cmd); err != nil || receipt.Rejected() {
		t.Fatalf("submit: %+v %v", receipt, err)
	}
	view := waitJournalRuntimeIdle(t, a)
	if view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatal(view.LastTurn)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 {
		t.Fatal("provider request missing")
	}
	found := false
	for _, msg := range provider.requests[0].Messages {
		if msg.Role != "user" {
			continue
		}
		// Prepared providers receive an owned JSON-normalized request.
		encoded, _ := json.Marshal(msg.Content)
		var parts []llm.ContentPart
		if err := json.Unmarshal(encoded, &parts); err != nil {
			t.Fatalf("image sent as plain text: %s", encoded)
		}
		for _, part := range parts {
			if part.ImageURL != nil && part.ImageURL.URL == "data:image/png;base64,"+base64.StdEncoding.EncodeToString(img.Data) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("provider did not receive exact image bytes")
	}
}

func TestClipboardInputDurableQueueReplayAndIdentity(t *testing.T) {
	cfg := testPersistenceConfig(t)
	a, err := NewWithOptions(&cfg, nil, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	local, pasted := inputPNG(t, 1), inputPNG(t, 2)
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "local.png"), local.Data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := protocol.NewSubmitInput("queued-images", protocol.SessionID(a.SessionID()), "image-input", "compare ![local](local.png)", protocol.InputFollowup)
	cmd.Input.Images = []protocol.InputImage{pasted}
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()
	receipt := a.applySubmitInput(cmd)
	a.mu.Lock()
	a.busy = false
	a.mu.Unlock()
	if receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(&cfg, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resumed.Close)
	if receipt := resumed.applySubmitInput(cmd); receipt.Rejected() {
		t.Fatalf("same typed input rejected after restart: %+v", receipt)
	}
	changed := cmd
	copy := *cmd.Input
	changed.Input = &copy
	changed.Input.Images = []protocol.InputImage{inputPNG(t, 3)}
	if receipt := resumed.applySubmitInput(changed); !receipt.Rejected() {
		t.Fatal("different image reused input ID")
	}
	resumed.mu.Lock()
	pending := resumed.pendingInputs[0]
	frozen := resumed.inputAttachments[pending.ID]
	resumed.mu.Unlock()
	msg := messages.Message{Role: messages.RoleUser, Content: pending.Text, ImagesFrozen: true, ImageAttachments: frozen.Attachments}
	parts, _, hasImage, err := resumed.imageContentPartsForMessage(cfg.Workspace, msg, maxAttachmentBytes)
	if err != nil || !hasImage {
		t.Fatalf("replay: %v", err)
	}
	var urls []string
	for _, part := range parts {
		if part.ImageURL != nil {
			urls = append(urls, part.ImageURL.URL)
		}
	}
	if len(urls) != 2 || urls[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(local.Data) || urls[1] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pasted.Data) {
		t.Fatal("mixed local/clipboard replay lost or reordered bytes")
	}
	records, err := resumed.persistenceHandle().Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if queued, ok := record.Event.(*session.InputQueued); ok && queued.InputID == "image-input" {
			if len(queued.Attachments) != 2 || len(queued.InputDigest) != 64 {
				t.Fatal("missing attachment blob refs or input digest")
			}
			if strings.Contains(queued.Text, base64.StdEncoding.EncodeToString(pasted.Data)) {
				t.Fatal("raw image leaked into transcript")
			}
		}
	}
}

func TestClipboardInputCannotBypassLocalPathChecks(t *testing.T) {
	a := newAgentForTest(t)
	defer a.Close()
	for _, text := range []string{"![escape](../outside.png)", "![forged](ccdp-clipboard:1)"} {
		cmd := protocol.NewSubmitInput("invalid", protocol.SessionID(a.SessionID()), "invalid", text, protocol.InputSteer)
		cmd.Input.Images = []protocol.InputImage{inputPNG(t, 255)}
		if receipt := a.applySubmitInput(cmd); !receipt.Rejected() {
			t.Fatal("untrusted path accepted")
		}
	}
}
