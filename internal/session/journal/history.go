package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/replay"
)

func (p *Writer) projectionLocked() (*replay.State, error) {
	if p == nil || p.store == nil {
		return nil, session.ErrClosed
	}
	cursor := p.store.CurrentCursor()
	if p.projection != nil && p.projectionCursor == cursor {
		return p.projection, nil
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		return nil, err
	}
	projection, err := replay.RecordsRaw(records)
	if err != nil {
		return nil, err
	}
	p.installProjection(projection)
	p.projectionCursor = cursor
	return projection, nil
}

// persistHistoryMessages incrementally appends the typed AssistantCommitted
// facts that represent the current accepted history. It is prefix-aware, so
// repeated history observations do not duplicate facts.
func (p *Writer) PersistHistoryMessages(history []messages.Message, turnID string, stepIDs ...string) error {
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
	// Runtime history may have been stamped with a local identity before the
	// durable InputQueued fact was reconciled. Match by message value at the
	// same occurrence and carry the durable identity forward; this preserves
	// distinct duplicate texts while making replay/UI IDs converge.
	for i := 0; i < len(projection.History) && i < len(history); i++ {
		if messageIdentityEquivalent(projection.History[i], history[i]) && projection.History[i].ID != "" {
			history[i].ID = projection.History[i].ID
		}
	}
	common := commonMessagePrefix(projection.History, history)
	if common == len(history) && len(projection.History) == len(history) {
		return nil
	}
	// A shrink or an edit is represented as a new projection boundary; facts
	// already in the log remain intact for audit/replay.
	var events []session.Event
	if common != len(projection.History) {
		baseCursor := p.store.CurrentCursor()
		txID := messages.StableID("history", struct {
			Cursor  session.Cursor
			TurnID  string
			History []messages.Message
		}{Cursor: baseCursor, TurnID: turnID, History: history})
		resetID := messages.StableID("history-reset", struct {
			Cursor session.Cursor
			TxID   string
		}{Cursor: baseCursor, TxID: txID})
		replacement, err := p.historyReplacementLocked(history, resetID, turnID, stepIDs...)
		if err != nil {
			return err
		}
		_, err = p.commitLocked(session.Batch{TransactionID: txID, Events: []session.Event{replacement}})
		return err
	}
	if events == nil {
		events = make([]session.Event, 0, len(history)-common)
	}
	stepID := ""
	if len(stepIDs) > 0 {
		stepID = stepIDs[0]
	}
	for _, message := range history[common:] {
		converted, err := p.messageToSessionLocked(message)
		if err != nil {
			return err
		}
		events = append(events, session.AssistantCommitted{TurnID: turnIDOrLegacy(turnID), StepID: stepID, Message: converted, ToolCalls: session.MessageToolCalls(converted)})
	}
	if len(events) == 0 {
		return nil
	}
	baseCursor := p.store.CurrentCursor()
	txID := messages.StableID("history", struct {
		Cursor  session.Cursor
		TurnID  string
		History []messages.Message
	}{Cursor: baseCursor, TurnID: turnID, History: history})
	_, err = p.commitLocked(session.Batch{TransactionID: txID, Events: events})
	return err
}

func (p *Writer) historyReplacementLocked(history []messages.Message, id, turnID string, stepIDs ...string) (session.ConversationReset, error) {
	event := session.ConversationReset{ResetID: id, Reason: "agent history projection replaced", TurnID: turnIDOrLegacy(turnID)}
	if len(stepIDs) > 0 {
		event.StepID = stepIDs[0]
	}
	for _, message := range history {
		converted, err := p.messageToSessionLocked(message)
		if err != nil {
			return event, err
		}
		event.Messages = append(event.Messages, converted)
	}
	return event, nil
}

// Validate the exact event codec and transaction limits before restoring files.
// A history replacement is one fact, so its size limit is independent of the
// number of messages. Oversized histories fail without modifying the workspace.
func (p *Writer) ValidateHistoryReplacement(history []messages.Message, turnID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	event, err := p.historyReplacementLocked(history, messages.StableID("history-reset", nil), turnID)
	if err != nil {
		return err
	}
	return session.ValidateBatch(session.Batch{TransactionID: messages.StableID("history", nil), Events: []session.Event{event}})
}

// initializeHistory is only for an unpublished fresh session. Normal turn
// projections retain their single-transaction atomicity.
func (p *Writer) InitializeHistory(ctx context.Context, history []messages.Message, turnID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if len(projection.History) != 0 {
		return errors.New("history initialization requires a fresh session")
	}
	var batch []session.Event
	size := int64(4096)
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		_, err := p.commitLocked(session.Batch{TransactionID: fmt.Sprintf("initialize-history-%d", p.store.CurrentCursor()), Events: batch})
		batch = nil
		size = 4096
		return err
	}
	for _, message := range history {
		if err := ctx.Err(); err != nil {
			return err
		}
		converted, err := p.messageToSessionLocked(message)
		if err != nil {
			return err
		}
		event := session.AssistantCommitted{TurnID: turnIDOrLegacy(turnID), Message: converted, ToolCalls: session.MessageToolCalls(converted)}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		// Include the wire union wrapper and transaction envelope overhead.
		eventSize := int64(len(data) + 1024)
		if eventSize+4096 > session.DefaultMaxTransactionBytes {
			return fmt.Errorf("%w: history message exceeds transaction byte limit", session.ErrTooLarge)
		}
		if len(batch) >= session.DefaultMaxEventsPerTransaction || size+eventSize > session.DefaultMaxTransactionBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, event)
		size += eventSize
	}
	return flush()
}

func commonMessagePrefix(left, right []messages.Message) int {
	n := len(left)
	if len(right) < n {
		n = len(right)
	}
	for i := 0; i < n; i++ {
		if !messagesEquivalent(left[i], right[i]) {
			return i
		}
	}
	return n
}
func messagesEquivalent(left, right messages.Message) bool {
	if (left.Role == messages.RoleUser && right.Role == messages.RoleUser) ||
		(left.Role == messages.RoleTool && right.Role == messages.RoleTool) {
		// InputQueued records acceptance time, while the legacy loop stamps the
		// same user message again when runTurn appends it.  The durable queue
		// timestamp is the canonical one; do not rewrite a whole history merely
		// because those two admission timestamps differ.  ToolResultsProjected
		// likewise has a durable, deterministic ID and no runtime timestamp;
		// the live message is initially stamped by NewToolResult.  Both are the
		// same model-visible result when call identity and content agree.
		left.ID = ""
		right.ID = ""
		left.CreatedAt = time.Time{}
		right.CreatedAt = time.Time{}
	}
	return messageFingerprint(left) == messageFingerprint(right)
}
func messageIdentityEquivalent(left, right messages.Message) bool {
	left.ID = ""
	right.ID = ""
	if (left.Role == messages.RoleUser && right.Role == messages.RoleUser) ||
		(left.Role == messages.RoleTool && right.Role == messages.RoleTool) {
		left.CreatedAt = time.Time{}
		right.CreatedAt = time.Time{}
	}
	return messageFingerprint(left) == messageFingerprint(right)
}
func messageFingerprint(message messages.Message) string {
	data, _ := json.Marshal(message)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// messageToSessionLocked materializes frozen image bytes into the scoped blob
// store before the enclosing AssistantCommitted/InputDelivered projection is
// committed. The caller must hold p.mu.
func (p *Writer) messageToSessionLocked(message messages.Message) (session.Message, error) {
	for i := range message.ImageAttachments {
		attachment := &message.ImageAttachments[i]
		if attachment.BlobHash != "" {
			if err := ValidateBlobRef(session.BlobRef{Hash: attachment.BlobHash, Size: attachment.BlobSize, MediaType: attachment.MediaType}); err != nil {
				return session.Message{}, err
			}
			continue
		}
		ref, err := p.putBlobLocked(context.Background(), attachment.Data, attachment.MediaType)
		if err != nil {
			return session.Message{}, fmt.Errorf("store image attachment %q: %w", attachment.Path, err)
		}
		if ref == nil {
			return session.Message{}, errors.New("agent: image attachment was not stored")
		}
		attachment.BlobHash, attachment.BlobSize, attachment.MediaType = ref.Hash, ref.Size, ref.MediaType
	}
	return messageToSession(message)
}

func messageToSession(m messages.Message) (session.Message, error) {
	messages.EnsureID(&m)
	return session.EncodeMessage(m)
}
func inputMessageID(id string) string { return protocol.InputMessageID(protocol.InputID(id)) }
func turnIDOrLegacy(id string) string {
	if strings.TrimSpace(id) == "" {
		return "turn-legacy"
	}
	return id
}
