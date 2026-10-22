package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"ccdp/internal/fsops"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/session"
)

// imageExts are the image formats we attach as multi-modal content parts.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// markdownImageRe matches ![alt](path) image references.
var markdownImageRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

const (
	maxImageBytes      = 20 * 1024 * 1024
	maxAttachmentBytes = 40 * 1024 * 1024
)

// freezeImageAttachments captures each authorized local image exactly once at
// input admission. Later requests use these bytes, not a changed source file.
func (a *Agent) freezeImageAttachments(text string) ([]messages.ImageAttachment, error) {
	a.mu.Lock()
	workspaceDir := a.services.cfg.Workspace
	policy := a.services.sandbox.Snapshot()
	a.mu.Unlock()
	matches := markdownImageRe.FindAllStringSubmatchIndex(text, -1)
	attachments := make([]messages.ImageAttachment, 0, len(matches))
	var used int64
	for _, match := range matches {
		path := strings.TrimSpace(text[match[2]:match[3]])
		data, mediaType, err := imageBytesAtBudget(workspaceDir, path, maxAttachmentBytes-used, policy.ResolveRead)
		if err != nil {
			return nil, fmt.Errorf("capture image %q: %w", path, err)
		}
		attachments = append(attachments, messages.ImageAttachment{Path: path, MediaType: mediaType, Data: data})
		used += int64(len(data))
	}
	return attachments, nil
}

// imageContentPartsAtBudget expands local image references while preserving
// failed references as ordinary text. The byte budget is checked from stat
// metadata before reading and io.LimitReader caps the allocation if a file
// grows between stat and read. used is the number of bytes actually attached.
func imageContentPartsAtBudget(workspaceDir, text string, budget int64, authorize func(string) (string, error)) (parts []llm.ContentPart, used int64, hasImage bool, firstErr error) {
	matches := markdownImageRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, 0, false, nil
	}
	last := 0
	for _, m := range matches {
		// Text between the previous marker and this one.
		if m[0] > last {
			parts = append(parts, llm.ContentPart{Type: "text", Text: text[last:m[0]]})
		}
		marker := text[m[0]:m[1]]
		path := strings.TrimSpace(text[m[2]:m[3]])
		part, size, err := imagePartAtBudget(workspaceDir, path, budget-used, authorize)
		if err == nil && part != nil {
			parts = append(parts, *part)
			used += size
			hasImage = true
		} else {
			// Keep the original markdown in the model-visible input. A missing,
			// oversized, or unreadable image must never disappear silently.
			parts = append(parts, llm.ContentPart{Type: "text", Text: marker})
			if firstErr == nil {
				firstErr = err
			}
		}
		last = m[1]
	}
	if last < len(text) {
		parts = append(parts, llm.ContentPart{Type: "text", Text: text[last:]})
	}
	return parts, used, hasImage, firstErr
}

func imagePartAtBudget(workspaceDir, path string, budget int64, authorize func(string) (string, error)) (*llm.ContentPart, int64, error) {
	data, mime, err := imageBytesAtBudget(workspaceDir, path, budget, authorize)
	if err != nil {
		return nil, 0, err
	}
	return &llm.ContentPart{
		Type: "image_url",
		ImageURL: &struct {
			URL string `json:"url"`
		}{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
	}, int64(len(data)), nil
}

func imageBytesAtBudget(workspaceDir, path string, budget int64, authorize func(string) (string, error)) ([]byte, string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if !imageExts[ext] {
		return nil, "", fmt.Errorf("unsupported image extension %q", ext)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspaceDir, abs)
	}
	root, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, "", err
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("image path escapes workspace")
	}
	file, err := fsops.OpenAuthorizedRegular(root, abs, authorize)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if info.Size() > maxImageBytes {
		return nil, "", fmt.Errorf("image %q exceeds per-file limit", path)
	}
	if budget <= 0 || info.Size() > budget {
		return nil, "", fmt.Errorf("image %q exceeds total attachment limit", path)
	}
	readLimit := int64(maxImageBytes)
	if budget < readLimit {
		readLimit = budget
	}
	// Include one sentinel byte so a file that grows after Stat is rejected,
	// while never allocating beyond the remaining total attachment budget.
	data, err := io.ReadAll(io.LimitReader(file, readLimit+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("image %q exceeds per-file limit", path)
	}
	if int64(len(data)) > budget {
		return nil, "", fmt.Errorf("image %q exceeds total attachment limit", path)
	}
	mime := map[string]string{
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp",
	}[ext]
	return data, mime, nil
}

func (a *Agent) frozenImageBytes(attachment messages.ImageAttachment) ([]byte, error) {
	if attachment.BlobHash == "" {
		return append([]byte(nil), attachment.Data...), nil
	}
	p := a.persistenceHandle()
	if p == nil {
		return nil, errors.New("agent: image artifact store is unavailable")
	}
	artifacts, err := p.RequestArtifacts()
	if err != nil {
		return nil, err
	}
	return artifacts.Read(session.BlobRef{Hash: attachment.BlobHash, Size: attachment.BlobSize, MediaType: attachment.MediaType}, maxImageBytes)
}

func imageMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// imageContentPartsForMessage uses frozen attachment bytes for canonical
// messages. Legacy messages without ImagesFrozen retain the compatibility
// path-based expansion behavior.
func (a *Agent) imageContentPartsForMessage(workspaceDir string, message messages.Message, budget int64, authorize func(string) (string, error)) (parts []llm.ContentPart, used int64, hasImage bool, firstErr error) {
	if !message.ImagesFrozen {
		return imageContentPartsAtBudget(workspaceDir, message.Content, budget, authorize)
	}
	matches := markdownImageRe.FindAllStringSubmatchIndex(message.Content, -1)
	if len(matches) == 0 {
		return []llm.ContentPart{{Type: "text", Text: message.Content}}, 0, false, nil
	}
	last := 0
	attachmentIndex := 0
	for _, match := range matches {
		if match[0] > last {
			parts = append(parts, llm.ContentPart{Type: "text", Text: message.Content[last:match[0]]})
		}
		marker := message.Content[match[0]:match[1]]
		path := strings.TrimSpace(message.Content[match[2]:match[3]])
		found := -1
		for i := attachmentIndex; i < len(message.ImageAttachments); i++ {
			if message.ImageAttachments[i].Path == path || message.ImageAttachments[i].Path == "" {
				found = i
				break
			}
		}
		if found < 0 {
			parts = append(parts, llm.ContentPart{Type: "text", Text: marker})
			if firstErr == nil {
				firstErr = fmt.Errorf("frozen image %q has no captured attachment", path)
			}
			last = match[1]
			continue
		}
		attachmentIndex = found + 1
		attachment := message.ImageAttachments[found]
		data, err := a.frozenImageBytes(attachment)
		if err == nil && (budget-used) > 0 && int64(len(data)) <= budget-used {
			mediaType := attachment.MediaType
			if mediaType == "" {
				mediaType = imageMediaType(path)
			}
			parts = append(parts, llm.ContentPart{Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)}})
			used += int64(len(data))
			hasImage = true
		} else {
			parts = append(parts, llm.ContentPart{Type: "text", Text: marker})
			if err == nil {
				err = fmt.Errorf("image %q exceeds total attachment limit", path)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		last = match[1]
	}
	if last < len(message.Content) {
		parts = append(parts, llm.ContentPart{Type: "text", Text: message.Content[last:]})
	}
	return parts, used, hasImage, firstErr
}
