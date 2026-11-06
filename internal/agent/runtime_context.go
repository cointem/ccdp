package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/workspace"
)

// Runtime sections are immutable history entries. Only a changed section is
// appended, preserving the complete previous request as a cacheable prefix.
// A repository snapshot belongs to a real user input and a read policy, not to
// tool completions or goal continuation messages.
func (step *stepRuntime) loadRuntimeContext(memory string, blocked bool) {
	previous := make(map[string]messages.Message)
	var input messages.Message
	for i := len(step.history) - 1; i >= 0; i-- {
		m := step.history[i]
		if strings.HasPrefix(m.InternalSource, "runtime_") {
			if _, seen := previous[m.InternalSource]; !seen {
				previous[m.InternalSource] = m
			}
		}
		if input.Role == "" && m.Role == messages.RoleUser && m.InternalSource == "" {
			input = m
		}
	}
	add := func(section, key, value string) {
		source := "runtime_" + section
		old, exists := previous[source]
		if value == "" {
			if !exists {
				return
			}
			value = "None."
		}
		content := "<runtime_context section=\"" + section + "\">\n" + value + "\n</runtime_context>"
		if exists && old.Content == content && old.ContextKey == key {
			return
		}
		m := messages.Message{Role: messages.RoleUser, InternalSource: source, ContextKey: key,
			Content: content, ImagesFrozen: true, CreatedAt: time.Now().UTC()}
		messages.EnsureID(&m)
		step.contextMessages = append(step.contextMessages, m)
	}
	access := fmt.Sprintf("Permission mode: %s. Outbound network=%t; local development TCP=%t. HOME retains tool discovery. Temporary files are owner-private; build and package caches persist for this project and access scope. Global installs, host credentials and host Unix sockets require explicit access.",
		step.cfg.PermissionMode, step.policy.NetworkAllowed(), step.policy.LocalDevelopmentNetwork)
	if step.cfg.ComputerUseEnabled {
		access += "\nComputer is enabled for this root session: macOS main display only. Use screenshot then act with its frame.id; existing file/API tools are preferred when applicable. Desktop access is separate from project execution."
	}
	if blocked {
		access += "\nProject execution is unavailable pending environment cleanup. Conversation, saved outputs, diagnostics and process cleanup remain available. Do not retry project operations or request broader access."
	}
	add("access", "", access)
	if !blocked {
		policy := step.policy
		// Runtime paths and scratch revisions do not affect repository visibility.
		scope, _ := json.Marshal(struct {
			Workspace, InputID, Query                          string
			Denied, Private, Credentials, ReadOnly, Additional []string
		}{step.cfg.Workspace, input.ID, input.Content, policy.DisallowedDirs, policy.PrivateRoots,
			policy.CredentialDirs, policy.ReadOnlyDirs, policy.AdditionalDirs})
		key := fmt.Sprintf("%x", sha256.Sum256(scope))
		if previous["runtime_repository"].ContextKey != key {
			if info, err := workspace.CachedScan(step.cfg.Workspace, false); err == nil && info != nil {
				layout := info.RelevantRepoMap(120, input.Content, func(path string) bool {
					_, err := policy.ResolveRead(filepath.Join(step.cfg.Workspace, path))
					return err == nil
				})
				add("repository", key, "# Repository layout\nSnapshot at this input boundary; use Glob/LS/Rg for current paths and contents.\n"+layout)
			}
		}
	}
	add("memory", "", memory)
	add("todo", "", step.todoPrompt)
	add("goal", "", step.goalPrompt)
	if step.modelSwitchMsg != "" {
		add("model", "", step.modelSwitchMsg)
	}
	if step.interruptNote {
		// Two interruptions with the same text are distinct events.
		add("interrupt", fmt.Sprintf("%d", step.turn), "The previous turn was interrupted. Tools may have partially executed. Verify affected state before continuing.")
	}
}
