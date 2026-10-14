package agent

import (
	"errors"
	"fmt"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (a *Agent) applySubmitInput(cmd protocol.Command) protocol.Receipt {
	input := *cmd.Input
	if err := protocol.ValidateInputImages(input.Images); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	// applySubmitInput is also used by the bounded /apply worker after it has
	// read a plan file, so it can be reached without a second Command.Normalize
	// call. Keep the protocol admission limit at this inner boundary as well;
	// otherwise that compatibility path could bypass the user-input bound.
	if len(input.Text) > protocol.MaxSubmitInputTextBytes {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand,
			fmt.Sprintf("input text exceeds %d bytes", protocol.MaxSubmitInputTextBytes))
	}
	inputID := input.ID
	if inputID == "" {
		inputID = protocol.InputID(cmd.ID)
	}
	input.ID = inputID
	if input.Strategy == "" {
		input.Strategy = protocol.InputSteer
	}
	inputDigest, digestErr := submitInputDigest(input)
	if digestErr != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "encode input identity: "+digestErr.Error())
	}
	createdAt := time.Now().UTC()

	// Queue admission, durable delivery, and the corresponding history append
	// share one Agent projection barrier. Turn settlement cannot observe a
	// half-admitted input or overwrite the just-committed history cache.
	a.mu.Lock()
	if old, ok := a.inbox.seenInputs[inputID]; ok {
		prior := a.inbox.seenInputBody[inputID]
		if prior != inputDigest {
			a.mu.Unlock()
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "input id was already used for different text or strategy")
		}
		a.mu.Unlock()
		return old
	}
	turnSeq := a.sequences.turnSeq
	closing := a.lifecycle.closing || a.lifecycle.closed
	if !a.turn.busy && !a.lifecycle.settling && input.Strategy != protocol.InputMessage {
		// Initialize the new turn at admission, before any durable I/O. A
		// cancellation delivered after this lock must remain set through the
		// eventual start; runTurn deliberately does not reset it.
		a.turn.interruptFlag = false
	}
	a.mu.Unlock()
	if closing {
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closed")
	}
	if err := a.persistenceFailure(); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, fmt.Sprintf("session persistence unavailable: %v", err))
	}
	if turnSeq == 0 {
		turnSeq = 1
	}
	// Capture all referenced images at the durable-input boundary. The path is
	// only an input locator; subsequent requests use the frozen bytes/blob.
	var attachments []messages.ImageAttachment
	var captureErr error
	if input.Strategy != protocol.InputMessage {
		attachments, captureErr = a.freezeImageAttachments(input.Text)
	}
	if captureErr != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, captureErr.Error())
	}
	// Clipboard locators are generated only after all user-supplied Markdown
	// paths have passed workspace checks. They are never opened as files.
	var imageBytes int
	for _, attachment := range attachments {
		imageBytes += len(attachment.Data)
	}
	for i, img := range input.Images {
		imageBytes += len(img.Data)
		if imageBytes > maxAttachmentBytes {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "images exceed total attachment limit")
		}
		path := fmt.Sprintf("ccdp-clipboard:%d", i+1)
		if input.Text != "" {
			input.Text += "\n"
		}
		input.Text += fmt.Sprintf("![Image %d](%s)", i+1, path)
		attachments = append(attachments, messages.ImageAttachment{Path: path, MediaType: img.MediaType, Data: append([]byte(nil), img.Data...)})
	}
	if err := a.persistInputWithDigest(string(inputID), input.Text, string(input.Strategy), fmt.Sprintf("turn-%d", turnSeq), createdAt, inputDigest, attachments...); err != nil {
		if errors.Is(err, session.ErrTooLarge) {
			// Input text is user-controlled data. A JSONL transaction which is
			// too large is a recoverable admission rejection (including escaped
			// control/HTML bytes that expand during JSON encoding), not evidence
			// that the session log is broken. Do not latch the runtime.
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "input exceeds the session transaction size limit")
		}
		a.markPersistenceFailure(err)
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, fmt.Sprintf("persist input: %v", err))
	}

	a.mu.Lock()
	// A turn may settle while the durable admission is in flight. Re-read the
	// owner state after the commit so the input is either queued for that turn
	// or starts the next one, but never starts before it is durable.
	if a.lifecycle.closing || a.lifecycle.closed {
		a.mu.Unlock()
		// InputQueued is already durable, even if shutdown wins before delivery.
		r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
		a.rememberSeenInput(inputID, inputDigest, r)
		return r
	}
	if a.turn.busy || a.lifecycle.settling || a.capabilities.settingsPreparing || a.capabilities.capabilityRevoking || input.Strategy == protocol.InputMessage {
		if a.inbox.inputAttachments == nil {
			a.inbox.inputAttachments = make(map[protocol.InputID]frozenInputAttachments)
		}
		a.inbox.inputAttachments[inputID] = frozenInputAttachments{Attachments: messages.CloneImageAttachments(attachments), Frozen: true}
		a.signalAgentActivityLocked()
		a.mu.Unlock()
		a.emitStatus("queued (will be delivered at a safe turn boundary)")
		r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
		a.rememberSeenInput(inputID, inputDigest, r)
		a.publishState()
		return r
	}
	nextTurnID := fmt.Sprintf("turn-%d", a.sequences.turnSeq+1)
	a.mu.Unlock()
	if err := a.persistInputDelivered(string(inputID), nextTurnID); err != nil {
		a.markPersistenceFailure(err)
		r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
		a.rememberSeenInput(inputID, inputDigest, r)
		return r
	}
	if err := a.persistenceHandle().ValidateProjection(); err != nil {
		a.markPersistenceFailure(err)
		r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
		a.rememberSeenInput(inputID, inputDigest, r)
		return r
	}
	turnInput := newTurnInput(inputID, input.Text, input.Strategy, createdAt)
	turnInput.ImageAttachments = messages.CloneImageAttachments(attachments)
	turnInput.ImagesFrozen = true
	turnInput.historyAppended = true
	turnInput.historyNotificationPending = true
	a.startTurnInput(turnInput)
	r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
	a.rememberSeenInput(inputID, inputDigest, r)
	return r
}

// rememberSeenInput records the receipt and body digest for a durably admitted
// input so a resumed retry of the same input id is deduplicated.
func (a *Agent) rememberSeenInput(inputID protocol.InputID, inputDigest string, r protocol.Receipt) {
	a.mu.Lock()
	a.inbox.seenInputs[inputID] = r
	a.inbox.seenInputBody[inputID] = inputDigest
	a.mu.Unlock()
}
