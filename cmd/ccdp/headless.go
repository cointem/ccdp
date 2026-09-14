package main

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"ccdp/internal/agent"
	"ccdp/internal/protocol"
)

// outputFormat selects the headless output style: text | json | stream-json.
type outputFormat string

const (
	outText       outputFormat = "text"
	outJSON       outputFormat = "json"
	outStreamJSON outputFormat = "stream-json"
)

var errHeadlessWatchTimeout = errors.New("headless: watch startup timed out")

var maxStreamJSONLineBytes = 4 << 20

// turnSink is used synchronously by the headless Watch loop. Keeping all
// writes on the caller goroutine avoids races and makes cancellation close
// deterministic even when a subscriber is slow.
type turnSink struct {
	text  strings.Builder
	err   error
	usage protocol.UsageSnapshot
}

var headlessCommandSeq uint64
var headlessProcessNonce = func() string {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}()

func nextHeadlessCommandID(prefix string) protocol.CommandID {
	return protocol.CommandID(fmt.Sprintf("headless-%s-%s-%d", headlessProcessNonce, prefix, atomic.AddUint64(&headlessCommandSeq, 1)))
}

// headlessWatch owns exactly one runtime subscription at a time. A resync
// marker closes the old bounded stream and opens a fresh stream whose first
// item is an atomic snapshot; a closed channel is terminal for this adapter
// and never causes a retry loop.
type headlessWatch struct {
	client      protocol.SessionClient
	sub         protocol.Subscription
	watchCancel context.CancelFunc
	cursor      protocol.Cursor
	snapshot    protocol.SessionView
}

func (w *headlessWatch) open(ctx context.Context) error {
	return w.openWithDeadline(ctx, ctx)
}

// openWithDeadline keeps the subscription lifetime on lifetimeCtx while
// bounding only the potentially blocking Watch constructor with startupCtx.
// Passing startupCtx directly to a real runtime Watch would make its watcher
// close as soon as the initial snapshot deadline is released.
func (w *headlessWatch) openWithDeadline(lifetimeCtx, startupCtx context.Context) error {
	// Watch is expected to honor context. Run the potentially blocking
	// constructor behind a bounded call so a broken adapter cannot hang
	// headless startup forever; the result channel is buffered so the worker
	// can always finish after cancellation.
	callCtx, cancel := context.WithCancel(lifetimeCtx)
	result := make(chan struct {
		sub protocol.Subscription
		err error
	}, 1)
	go func() {
		sub, err := w.client.Watch(callCtx, w.cursor)
		result <- struct {
			sub protocol.Subscription
			err error
		}{sub: sub, err: err}
	}()
	timerDuration := 5 * time.Second
	if deadline, ok := startupCtx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timerDuration {
			if remaining <= 0 {
				timerDuration = time.Nanosecond
			} else {
				timerDuration = remaining
			}
		}
	}
	timer := time.NewTimer(timerDuration)
	defer timer.Stop()
	select {
	case outcome := <-result:
		if outcome.err != nil {
			cancel()
			return outcome.err
		}
		w.sub = outcome.sub
		w.watchCancel = cancel
		return nil
	case <-startupCtx.Done():
		cancel()
		outcome := <-result
		if outcome.sub != nil {
			_ = outcome.sub.Close()
		}
		return startupCtx.Err()
	case <-timer.C:
		cancel()
		outcome := <-result
		if outcome.sub != nil {
			_ = outcome.sub.Close()
		}
		return errHeadlessWatchTimeout
	}
}

func (w *headlessWatch) close() {
	if w.watchCancel != nil {
		w.watchCancel()
		w.watchCancel = nil
	}
	if w.sub != nil {
		_ = w.sub.Close()
		w.sub = nil
	}
}

func (w *headlessWatch) next(ctx context.Context) (protocol.Update, error) {
	if w.sub == nil {
		return protocol.Update{}, errors.New("headless: watch is closed")
	}
	select {
	case <-ctx.Done():
		return protocol.Update{}, ctx.Err()
	case update, ok := <-w.sub.Updates():
		if !ok {
			return protocol.Update{}, errors.New("headless: watch closed")
		}
		w.cursor = update.Cursor
		if update.Snapshot != nil {
			w.snapshot = *update.Snapshot
		}
		return update, nil
	}
}

func (w *headlessWatch) resync(lifetimeCtx, startupCtx context.Context, marker protocol.Update) error {
	w.close()
	w.cursor = marker.Cursor
	return w.openWithDeadline(lifetimeCtx, startupCtx)
}

// runHeadless drives the protocol client for one or more prompts without the
// TUI. Input, approval, plan decisions, and cancellation all use
// SessionClient.Submit.
//
// Returns process exit code (0 success, 1 runtime/LLM error, 2 timeout).
func runHeadless(ag *agent.Agent, prompts []string, format outputFormat, timeoutSec int) int {
	if len(prompts) == 0 {
		return 0
	}
	if ag == nil {
		return headlessOutputSnapshot(protocol.SessionView{}, format, "", protocol.UsageSnapshot{}, errors.New("session protocol is unavailable"), 1)
	}
	return runHeadlessClient(ag, prompts, format, timeoutSec)
}

// runHeadlessClient is the protocol-only entry point. The Agent wrapper above
// preserves the CLI surface while tests can exercise the same Watch/Submit
// lifecycle with a small SessionClient implementation.
func runHeadlessClient(client protocol.SessionClient, prompts []string, format outputFormat, timeoutSec int) int {
	if len(prompts) == 0 {
		return 0
	}
	if client == nil {
		return headlessOutputSnapshot(protocol.SessionView{}, format, "", protocol.UsageSnapshot{}, errors.New("session protocol is unavailable"), 1)
	}
	if timeoutSec <= 0 {
		timeoutSec = 600
	}
	// -timeout is the lifetime budget for headless processing: watch startup,
	// every prompt, resyncs and approvals share one deadline. A per-prompt
	// timeout would let a multi-prompt stream run for N*timeoutSec while
	// claiming to honor the flag. Cancellation cleanup has its own short,
	// bounded grace period in stopHeadlessTurn.
	totalTimeout := time.Duration(timeoutSec) * time.Second
	rootCtx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()

	watch := &headlessWatch{client: client}
	startupBudget := 5 * time.Second
	if totalTimeout < startupBudget {
		startupBudget = totalTimeout
	}
	startupCtx, startupCancel := context.WithTimeout(rootCtx, startupBudget)
	if err := watch.openWithDeadline(rootCtx, startupCtx); err != nil {
		startupCancel()
		code := 1
		if errors.Is(rootCtx.Err(), context.DeadlineExceeded) || errors.Is(err, errHeadlessWatchTimeout) {
			code = 2
			err = fmt.Errorf("timeout after %ds", timeoutSec)
		}
		return headlessOutputSnapshot(watch.snapshot, format, "", protocol.UsageSnapshot{}, err, code)
	}
	defer watch.close()
	// Watch emits its initial snapshot atomically. Do not use a separate
	// mutable Agent getter as the source of session identity or model.
	initial, err := watch.next(startupCtx)
	startupCancel()
	if err != nil {
		code := 1
		if errors.Is(rootCtx.Err(), context.DeadlineExceeded) {
			code = 2
			err = fmt.Errorf("timeout after %ds", timeoutSec)
		}
		return headlessOutputSnapshot(watch.snapshot, format, "", protocol.UsageSnapshot{}, err, code)
	}
	if initial.Snapshot != nil {
		watch.snapshot = *initial.Snapshot
	}
	sessionID := watch.snapshot.SessionID
	if format == outStreamJSON {
		emitJSONLine(map[string]any{
			"type": "message_start", "session_id": sessionID.String(), "model": watch.snapshot.Settings.Model.Model,
		})
	}

	var finalText string
	var finalUsage protocol.UsageSnapshot
	var finalErr error
	for _, prompt := range prompts {
		if strings.TrimSpace(prompt) == "" {
			continue
		}
		if errors.Is(rootCtx.Err(), context.DeadlineExceeded) {
			return headlessOutputSnapshot(watch.snapshot, format, finalText, finalUsage,
				fmt.Errorf("timeout after %ds", timeoutSec), 2)
		}
		baseline := len(watch.snapshot.History)
		sink := turnSink{usage: watch.snapshot.Usage}
		turnCtx, turnCancel := context.WithCancel(rootCtx)
		cmd := protocol.NewSubmitInput(nextHeadlessCommandID("input"), sessionID,
			protocol.InputID(nextHeadlessCommandID("input-id")), prompt, protocol.InputSteer)
		receipt, submitErr := client.Submit(turnCtx, cmd)
		if submitErr != nil {
			turnCancel()
			if errors.Is(rootCtx.Err(), context.DeadlineExceeded) || errors.Is(submitErr, context.DeadlineExceeded) {
				stopHeadlessTurn(client, sessionID)
				return headlessOutputSnapshot(watch.snapshot, format, finalText, finalUsage,
					fmt.Errorf("timeout after %ds", timeoutSec), 2)
			}
			finalErr = submitErr
			break
		}
		if receipt.Rejected() {
			turnCancel()
			finalErr = receiptError(receipt)
			break
		}

		turnErr := consumeHeadlessTurn(turnCtx, rootCtx, watch, sessionID, format, &sink, baseline, client)
		turnCancel()
		if turnErr != nil {
			if errors.Is(rootCtx.Err(), context.DeadlineExceeded) || errors.Is(turnErr, context.DeadlineExceeded) {
				stopHeadlessTurn(client, sessionID)
				finalText, finalUsage = sink.text.String(), sink.usage
				return headlessOutputSnapshot(watch.snapshot, format, finalText, finalUsage,
					fmt.Errorf("timeout after %ds", timeoutSec), 2)
			}
			finalErr = turnErr
			break
		}
		if sink.err != nil {
			finalErr = sink.err
		}
		finalText, finalUsage = sink.text.String(), sink.usage
		if format == outStreamJSON {
			emitJSONLine(map[string]any{
				"type": "message_stop",
				"usage": map[string]any{
					"input_tokens":  finalUsage.InputTokens,
					"output_tokens": finalUsage.OutputTokens,
					"cost":          finalUsage.Cost,
				},
			})
		}
		if finalErr != nil {
			break
		}
	}

	if finalErr != nil {
		return headlessOutputSnapshot(watch.snapshot, format, finalText, finalUsage, finalErr, 1)
	}
	return headlessOutputSnapshot(watch.snapshot, format, finalText, finalUsage, nil, 0)
}

func receiptError(receipt protocol.Receipt) error {
	if receipt.Error != nil {
		return errors.New(receipt.Error.Error())
	}
	return errors.New("command rejected")
}

func stopHeadlessTurn(client protocol.SessionClient, sessionID protocol.SessionID) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = client.Submit(ctx, protocol.Command{ID: nextHeadlessCommandID("stop"), SessionID: sessionID, Type: protocol.CommandStop})
}

func consumeHeadlessTurn(ctx, lifetimeCtx context.Context, watch *headlessWatch, sessionID protocol.SessionID, format outputFormat, sink *turnSink, baseline int, client protocol.SessionClient) error {
	cancelledQuestions := map[string]bool{}
	cancelQuestion := func(q *protocol.QuestionRequest) error {
		if q == nil || cancelledQuestions[q.ID] {
			return nil
		}
		if format == outStreamJSON {
			emitJSONLine(map[string]any{"type": "question_request", "question": q, "cancelled": true, "reason": "headless mode has no interactive answer channel"})
		}
		receipt, err := client.Submit(ctx, protocol.Command{ID: nextHeadlessCommandID("answer"), SessionID: sessionID, Type: protocol.CommandAnswerQuestion, Answer: &protocol.AnswerQuestion{RequestID: q.ID, Cancelled: true}})
		if err != nil {
			return err
		}
		if receipt.Rejected() && (receipt.Error == nil || receipt.Error.Code != protocol.ErrorNotFound) {
			return receiptError(receipt)
		}
		cancelledQuestions[q.ID] = true
		return nil
	}

	for {
		update, err := watch.next(ctx)
		if err != nil {
			return err
		}
		if update.Snapshot != nil {
			watch.snapshot = *update.Snapshot
			sink.usage = watch.snapshot.Usage
			if err := cancelQuestion(watch.snapshot.Question); err != nil {
				return err
			}
		}
		if update.Child != nil {
			if format == outStreamJSON {
				emitJSONLine(map[string]any{"type": "child_update", "child": update.Child, "cursor": update.Cursor})
			}
			continue
		}
		if update.Type == protocol.UpdateResyncRequired {
			if format == outStreamJSON {
				return errors.New("headless: stream resynchronized before turn completion")
			}
			if err := watch.resync(lifetimeCtx, ctx, update); err != nil {
				return err
			}
			initial, err := watch.next(ctx)
			if err != nil {
				return err
			}
			if initial.Snapshot != nil {
				watch.snapshot = *initial.Snapshot
				sink.usage = watch.snapshot.Usage
				if err := cancelQuestion(watch.snapshot.Question); err != nil {
					return err
				}
			}
			if outcome := watch.snapshot.LastTurn; outcome != nil && outcome.Status != protocol.TurnRunning && outcome.Status != protocol.TurnSucceeded {
				message := outcome.Error
				if message == "" {
					message = "turn did not succeed after resync"
				}
				return errors.New(message)
			}
			// Text/JSON can recover a committed assistant result from the
			// snapshot. If no new confirmed history exists, keep waiting for
			// a terminal event rather than treating an idle snapshot as success.
			if !watch.snapshot.Busy && !headlessChildrenActive(ctx, client) && len(watch.snapshot.PendingInputs) == 0 && len(watch.snapshot.History) > baseline {
				if text := latestAssistant(watch.snapshot.History, baseline); text != "" {
					sink.text.Reset()
					sink.text.WriteString(text)
					return nil
				}
			}
			continue
		}
		if update.Event == nil || update.Event.SessionID != "" && update.Event.SessionID != sessionID {
			continue
		}
		ev := update.Event
		switch ev.Kind {
		case protocol.EventStream:
			sink.text.WriteString(ev.Text)
			if format == outStreamJSON {
				emitJSONLine(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
			}
		case protocol.EventReasoning:
			if format == outStreamJSON {
				emitJSONLine(map[string]any{"type": "reasoning_delta", "text": ev.Text})
			}
		case protocol.EventError:
			text := ev.Error
			if text == "" {
				text = ev.Text
			}
			sink.err = errors.New(text)
			if format == outStreamJSON {
				emitJSONLine(map[string]any{"type": "error", "error": text})
			}
		case protocol.EventUsage:
			if ev.Usage != nil {
				sink.usage = *ev.Usage
			}
		case protocol.EventToolStarted:
			if format == outStreamJSON && ev.Tool != nil {
				var input any
				_ = json.Unmarshal(ev.Tool.Args, &input)
				emitJSONLine(map[string]any{"type": "tool_use_start", "tool": ev.Tool.Name, "input": input})
			}
		case protocol.EventToolResult:
			if format == outStreamJSON && ev.Tool != nil {
				emitJSONLine(map[string]any{"type": "tool_result", "tool": ev.Tool.Name, "status": ev.Tool.Status, "output": ev.Tool.Output})
			}
		case protocol.EventApprovalRequest:
			if ev.Approval != nil {
				receipt, err := client.Submit(ctx, protocol.Command{ID: nextHeadlessCommandID("approval"), SessionID: sessionID, Type: protocol.CommandApproveTool,
					Approval: &protocol.ApproveTool{ApprovalID: ev.Approval.ID, Approve: false}})
				if err != nil {
					return err
				}
				if receipt.Rejected() {
					return receiptError(receipt)
				}
			}
		case protocol.EventQuestionRequest:
			if err := cancelQuestion(ev.Question); err != nil {
				return err
			}
		case protocol.EventPlanReady:
			if ev.Plan != nil {
				receipt, err := client.Submit(ctx, protocol.Command{ID: nextHeadlessCommandID("plan"), SessionID: sessionID, Type: protocol.CommandApprovePlan,
					Plan: &protocol.ApprovePlan{PlanID: ev.Plan.ID, PlanVersion: ev.Plan.Version, Approve: false}})
				if err != nil {
					return err
				}
				if receipt.Rejected() {
					return receiptError(receipt)
				}
			}
		case protocol.EventTurnDone:
			if headlessChildrenActive(ctx, client) || len(watch.snapshot.PendingInputs) > 0 {
				continue
			}
			return nil
		}
	}
}

func headlessChildrenActive(ctx context.Context, client protocol.SessionClient) bool {
	owner, ok := client.(interface {
		Sessions() protocol.SessionDirectory
	})
	if !ok || owner.Sessions() == nil {
		return false
	}
	rows, err := owner.Sessions().ListChildren(ctx)
	if err != nil {
		return true
	}
	for _, row := range rows {
		if row.Run.Active() || row.DeliveryPending {
			return true
		}
	}
	return false
}

func latestAssistant(history []protocol.MessageView, baseline int) string {
	if baseline < 0 {
		baseline = 0
	}
	for i := len(history) - 1; i >= baseline; i-- {
		if strings.EqualFold(history[i].Role, "assistant") {
			return history[i].Content
		}
	}
	return ""
}

// inputMsg is one frame of --input-format stream-json.
type inputMsg struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// parseStreamJSONInput reads newline-delimited input messages and returns the
// user texts. Supports string content or an array of {type:text,text} parts.
func parseStreamJSONInput(r *bufio.Reader) ([]string, error) {
	var prompts []string
	lineNumber := 0
	for {
		line, err := readBoundedLine(r, maxStreamJSONLineBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if line == "" && errors.Is(err, io.EOF) {
			break
		}
		lineNumber++
		line = strings.TrimSpace(line)
		if line != "" {
			var m inputMsg
			if jerr := json.Unmarshal([]byte(line), &m); jerr != nil {
				return nil, fmt.Errorf("stream-json input line %d: invalid JSON: %w", lineNumber, jerr)
			}
			stop := false
			switch m.Type {
			case "user":
				if s := contentText(m.Message.Content); s != "" {
					prompts = append(prompts, s)
				}
			case "assistant":
				stop = true
			}
			if stop {
				break
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if len(prompts) == 0 {
		return nil, errors.New("no user messages in stream-json input")
	}
	return prompts, nil
}

func readBoundedLine(r *bufio.Reader, limit int) (string, error) {
	if limit <= 0 {
		limit = 1
	}
	line := make([]byte, 0, min(limit, 4096))
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > limit {
			return "", fmt.Errorf("stream-json input line exceeds %d bytes", limit)
		}
		if err == nil {
			return string(line), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return "", io.EOF
			}
			return string(line), io.EOF
		}
		return "", err
	}
}

// contentText normalizes a message content payload to text.
func contentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// emitJSONLine writes one JSON object as a line to stdout (stream-json mode).
func emitJSONLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Println(string(b))
}

func headlessOutputSnapshot(snapshot protocol.SessionView, format outputFormat, result string, usage protocol.UsageSnapshot, err error, exitCode int) int {
	if format == outStreamJSON {
		if err != nil {
			emitJSONLine(map[string]any{"type": "error", "error": err.Error()})
		}
		return exitCode
	}
	sessionID := snapshot.SessionID.String()
	model := snapshot.Settings.Model.Model
	if format == outJSON {
		out := map[string]any{
			"result": result, "session_id": sessionID, "model": model,
			"usage": map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
				"cost": usage.Cost, "turns": usage.TurnCount},
		}
		if err != nil {
			out["error"] = err.Error()
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return exitCode
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccdp: "+err.Error())
		if result != "" {
			fmt.Println(result)
		}
		return exitCode
	}
	fmt.Println(result)
	return exitCode
}
