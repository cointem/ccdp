package agent

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"ccdp/internal/fsops"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
)

// restoreContextSnippet assembles a short "session context to restore" block
// appended to the compaction summary: discovered deferred tools, the active
// task list, and up to five files recently touched in the kept tail.
func (a *Agent) restoreContextSnippet(keptTail []messages.Message) string {
	var sb strings.Builder

	// Discovered deferred tools stay available after compaction.
	var disc []string
	for n := range a.discovery.deferTools {
		if a.isDiscovered(n) {
			disc = append(disc, n)
		}
	}
	if len(disc) > 0 {
		sort.Strings(disc)
		fmt.Fprintf(&sb, "Discovered tools available to you: %s\n", strings.Join(disc, ", "))
	}

	// Active task list (persisted across turns).
	a.mu.Lock()
	resources := a.services.resources
	a.mu.Unlock()
	if resources != nil && resources.Todos != nil {
		if sec, err := resources.Todos.Section(); err == nil && sec != "" {
			sb.WriteString(sec)
			sb.WriteString("\n")
		}
	}

	// Recently touched files, from tool arguments in the kept tail.
	seen := map[string]bool{}
	var files []string
	for _, m := range keptTail {
		for _, tc := range m.ToolCalls {
			for _, key := range []string{"file_path", "path", "file"} {
				if p, ok := tc.Arguments[key].(string); ok && p != "" && !seen[p] {
					seen[p] = true
					files = append(files, p)
				}
			}
		}
	}
	if len(files) > 5 {
		files = files[:5]
	}
	if len(files) > 0 {
		fmt.Fprintf(&sb, "Recently touched files: %s\n", strings.Join(files, ", "))
	}

	return strings.TrimRight(sb.String(), "\n")
}

// postCompactFileAttachments re-reads the most recently Read files (Claude
// Code's createPostCompactFileAttachments) and renders their content into the
// compaction summary, so the model keeps its working set after the older
// conversation — which contained those Reads — is summarized away. Files whose
// content is still visible in the kept tail are skipped, as are files that
// disappeared from disk. A per-file and total byte budget keeps the
// attachments from becoming a second compaction problem.
func (a *Agent) postCompactFileAttachments(keptTail []messages.Message) string {
	// Paths still referenced by the kept tail do not need re-attachment.
	inTail := map[string]bool{}
	for _, m := range keptTail {
		for _, tc := range m.ToolCalls {
			for _, key := range []string{"file_path", "path", "file"} {
				if p, ok := tc.Arguments[key].(string); ok {
					inTail[p] = true
				}
			}
		}
	}

	const (
		maxFiles   = 5
		maxPerFile = 8000  // chars of content per file
		maxTotal   = 24000 // chars across all attachments
	)
	var (
		sb    strings.Builder
		total int
		count int
	)
	a.mu.Lock()
	resources := a.services.resources
	a.mu.Unlock()
	var recentReads []tools.FileReadRecord
	a.mu.Lock()
	policy := a.services.sandbox.Snapshot()
	a.mu.Unlock()
	if resources != nil && resources.Files != nil {
		recentReads = resources.Files.RecentReads(16)
	}
	for _, rec := range recentReads {
		if count >= maxFiles || total >= maxTotal {
			break
		}
		if inTail[rec.Path] {
			continue
		}
		// A previous Read grants no future access after policy changes.
		path, err := policy.ResolveRead(rec.Path)
		if err != nil {
			continue
		}
		data, err := readBoundedRegularFile(path, maxPerFile+1, policy.ResolveRead)
		if err != nil {
			continue // deleted or unreadable since the Read
		}
		content := string(data)
		note := ""
		if info, serr := os.Stat(rec.Path); serr == nil && !info.ModTime().Equal(rec.Mtime) {
			note = " — file changed since it was read; excerpt of current content"
		}
		if len(content) > maxPerFile {
			content = truncateResult(content, maxPerFile)
		}
		if total+len(content) > maxTotal {
			content = truncateResult(content, maxTotal-total)
		}
		if content == "" && total >= maxTotal {
			break
		}
		if count == 0 {
			sb.WriteString("Post-compaction file attachments — content of files you read earlier in this conversation, re-attached so you do not have to Read them again:\n")
		}
		fmt.Fprintf(&sb, "\n<ccdp-file-attachment path=%q%s>\n%s\n</ccdp-file-attachment>\n", rec.Path, note, content)
		total += len(content) + 64
		count++
	}
	return strings.TrimRight(sb.String(), "\n")
}

// readBoundedRegularFile uses the shared nonblocking, regular-file open.
// The caller requests an extra byte to detect truncation.
func readBoundedRegularFile(path string, maxBytes int, authorize func(string) (string, error)) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("attachment byte limit must be positive")
	}
	f, err := fsops.OpenAuthorizedRegular("", path, authorize)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(maxBytes)))
}
