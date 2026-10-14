package journal

import (
	"context"
	"errors"
	"reflect"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

// persistInputQueued is the runtime's durable inbox admission helper.  It
// deliberately has no side effects beyond the synced Store commit.
func (p *Writer) PersistInputQueued(inputID, text string, strategy string, turnID string, createdAt time.Time, attachments ...messages.ImageAttachment) error {
	return p.PersistInputQueuedWithDigest(inputID, text, strategy, turnID, createdAt, "", attachments...)
}

func (p *Writer) PersistInputQueuedWithDigest(inputID, text string, strategy string, turnID string, createdAt time.Time, digest string, attachments ...messages.ImageAttachment) error {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	refs := make([]session.BlobRef, 0, len(attachments))
	for i := range attachments {
		if attachments[i].Data == nil && attachments[i].BlobHash == "" {
			continue
		}
		if attachments[i].BlobHash != "" {
			ref := session.BlobRef{Hash: attachments[i].BlobHash, Size: attachments[i].BlobSize, MediaType: attachments[i].MediaType}
			if err := ValidateBlobRef(ref); err != nil {
				return err
			}
			refs = append(refs, ref)
			continue
		}
		ref, err := p.PutBlob(context.Background(), attachments[i].Data, attachments[i].MediaType)
		if err != nil {
			return err
		}
		if ref == nil {
			return errors.New("agent: input image artifact was not stored")
		}
		attachments[i].BlobHash, attachments[i].BlobSize = ref.Hash, ref.Size
		attachments[i].MediaType = ref.MediaType
		refs = append(refs, *ref)
	}
	_, err := p.CommitEvents("input-queued-"+inputID, session.InputQueued{
		InputDigest: digest,
		InputID:     inputID, MessageID: inputMessageID(inputID), Text: text, Attachments: refs, ImagesFrozen: attachments != nil, Strategy: strategy, TurnID: turnID, CreatedAt: createdAt,
	})
	return err
}

func (p *Writer) PersistInputDelivered(inputID, turnID string) error {
	_, err := p.CommitEvents("input-delivered-"+inputID, session.InputDelivered{InputID: inputID, TurnID: turnID})
	return err
}

// persistSettingsSnapshot appends the complete public settings view captured
// at a command/step boundary, so replay never has to guess the active mode,
// sandbox policy, or pending-public binding from mutable Agent state.
func (p *Writer) PersistSettingsSnapshot(settings session.Settings, revision uint64) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persistSettingsSnapshotLocked(settings, revision)
}

func (p *Writer) persistSettingsSnapshotLocked(settings session.Settings, revision uint64) error {
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Settings, settings) && (revision == 0 || revision <= projection.SettingsRev) {
		return nil
	}
	if revision == 0 {
		revision = projection.SettingsRev + 1
		if revision == 0 {
			revision = 1
		}
	}
	baseCursor := p.store.CurrentCursor()
	_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("settings-snapshot", struct {
		Settings session.Settings
		Revision uint64
		Cursor   session.Cursor
	}{settings, revision, baseCursor}), Events: []session.Event{session.SettingsChanged{Revision: revision, Settings: settings}}})
	return err
}

// persistWorkflow appends a recoverable workflow state transition. Workflow
// is UI/session state, not model history, so replay keeps it in the typed
// projection without manufacturing a chat message.
func (p *Writer) PersistWorkflow(workflow session.WorkflowState) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Workflow, workflow) {
		return nil
	}
	_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("workflow", struct {
		Workflow session.WorkflowState
		Cursor   session.Cursor
	}{workflow, p.store.CurrentCursor()}), Events: []session.Event{session.WorkflowChanged{Workflow: workflow}}})
	return err
}

// persistMemory stores the bounded, complete memory projection as a typed
// fact. clear is explicit because an empty MemoryChanged payload is invalid;
// no-session mode therefore remains wholly in MemoryStore/artifacts and never
// falls back to the configured session directory.
func (p *Writer) PersistMemory(text string, clear bool) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if projection.MemorySeen && ((clear && projection.MemoryText == "") || (!clear && projection.MemoryText == text)) {
		return nil
	}
	event := session.MemoryChanged{Revision: p.store.CurrentCursor() + 1, Text: text, Cleared: clear}
	_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("memory", struct {
		Text   string
		Clear  bool
		Cursor session.Cursor
	}{text, clear, p.store.CurrentCursor()}), Events: []session.Event{event}})
	return err
}

// persistTasks records the full task-list replacement after TodoWrite has
// durably changed its resource. The typed event is the recovery authority;
// todos.json remains only a compatibility/cache representation.
func (p *Writer) PersistTasks(tasks []session.Task) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Tasks, tasks) {
		return nil
	}
	copyTasks := append([]session.Task(nil), tasks...)
	_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("tasks", struct {
		Tasks  []session.Task
		Cursor session.Cursor
	}{copyTasks, p.store.CurrentCursor()}), Events: []session.Event{session.TasksChanged{Revision: p.store.CurrentCursor() + 1, Tasks: copyTasks}}})
	return err
}

func (p *Writer) PersistToolsDiscovered(version uint64, schemas []session.ToolSchema) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	if version == 0 {
		version = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	allKnown := len(schemas) > 0
	for _, schema := range schemas {
		if _, ok := projection.Discovered[schema.ToolID]; !ok {
			allKnown = false
			break
		}
	}
	if allKnown {
		return nil
	}
	copySchemas := append([]session.ToolSchema(nil), schemas...)
	_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("tools-discovered", struct {
		Version uint64
		Tools   []session.ToolSchema
		Cursor  session.Cursor
	}{version, copySchemas, p.store.CurrentCursor()}), Events: []session.Event{session.ToolsDiscovered{CatalogVersion: version, Tools: copySchemas}}})
	return err
}

func (p *Writer) PersistCommandCompleted(event session.CommandCompleted) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	if event.CommandID == "" {
		return errors.New("agent: command completion id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	_, err := p.commitLocked(session.Batch{TransactionID: messages.StableID("command-completed", event), Events: []session.Event{event}})
	return err
}
