# TUI architecture

The default CLI uses the migrated TUI through `tui.NewProgram`. There is one
production renderer and one transcript projection. The implementation stays in
Go/Bubble Tea; the Codex reference and attribution live in `third_party/codex`.

## Ownership and data flow

`SessionClient → protocol reducer → CellStore / operations / notices / panels → frame + history transaction → TerminalHost`

| Component | Responsibility |
| --- | --- |
| `session_host.go` | The only frontend integration with concrete `agent.Agent`: creation, resume, retirement and CLI shutdown. |
| `protocol.go` | Applies identified cumulative transcript events, authoritative snapshots and command receipts. Raw stream/tool deltas never create a second transcript. |
| `CellStore` | Original text, typed tool metadata, stable identity, revisions, ordered reports and indexed replacement. The reader uses the same projection. |
| `historyLedger` | Terminal-independent ordered batch planner. Mutable cells block later commits; explicit `PreviousID` transitions reconcile committed and pending prefixes. |
| `historyDelivery` | Draft transaction, generation/epoch and batch identity. Publishes only a matching successful write acknowledgment. |
| `StreamController` | Markdown commit boundaries, batching policy and grapheme-safe text boundaries. Rendering does not own stream progress. |
| `TerminalHost` | Sole stdout boundary, serialized writes, private barrier removal, short-write/error handling and terminal mode restoration through Bubble Tea. Notifications also use this host. |
| `composerState` | Textarea, paste folds, attachments, history and composer rendering. The app coordinates submission and layout effects. |
| `noticeStore` / `operationStore` | One notification source and one command lifecycle record. Recoverable input text and attachments belong to the operation; no parallel pending-draft maps. |
| `PanelManager` / `Selector` | Shared display/focus priority and one typed selection/scroll state. Approval scrolling also has one state; opening a decision visibly focuses the first choice (Allow once / Approve plan). |
| `frameChrome` / layout | Shared measurement and rendering inputs, native chat history, full-height cached transcript viewport with bottom-anchored controls and explicit alternate-screen readers. |

The root `Model` is the Bubble Tea reducer: it coordinates effects between these
owners. Retaining reducer methods on `Model` does not introduce a second state
owner. Runtime objects are not imported into rendering/input/protocol modules.

## Removed migration paths

- Mirrored status string and `syncLegacyStatus`.
- Parallel picker labels/options/index and selector synchronization.
- `pendingSubmissions` and `pendingImages` maps.
- Id-less stream accumulation, ordinal/text identity inference and legacy ID bridges.
- Chat reconstruction from `SessionView.History`; `Transcript` is the display source.
- Queue-only terminal completion and unmediated notification stdout writes.
- Old centered decision/picker rendering, formatted tool-text parsing and unused helpers.
- Unused Gopher renderer, sprite images, rasterizer and dedicated preview tests.

`History` remains a protocol feature for operations such as export/rewind; it is
not an alternative chat renderer. Existing Markdown rendering, terminal-width
helpers, Bubble Tea widgets and business protocol semantics are retained.
A real unified diff is rendered when provided; Edit/Write arguments are explicitly
labeled previews, never fabricated file versions or line numbers.

## Verification

- Full `go test ./...` and `go vet ./internal/tui ./internal/agent`.
- Race checks for terminal/history transactions, identified protocol projection,
  cell updates, streaming and panel routing.
- `migration_contract_test.go` enforces runtime/output boundaries and retired
  migration symbols; behavioral tests cover ordered commits, pending ID changes,
  duplicate/replayed events, cancellation and session switching.
- Actual OS PTY with pyte at 80×24, 40×16 and 120×40: history, arrows, reader,
  Chinese drafts, resize and normal terminal restoration.
- Authorized live model + PTY: read temporary input.txt, approve its edit once,
  verify with cat, receive E2E_TUI_OK, wait for idle and exit with code 0.

Reproduce the deterministic PTY suite without a model:

```sh
go test -c -o /tmp/ccdp-tui.test ./internal/tui
# Install testdata/requirements-pty.txt into an isolated Python directory.
PYTHONPATH=<isolated-directory> python3 internal/tui/testdata/pty_smoke.py /tmp/ccdp-tui.test /tmp/ccdp-tui-pty-results.json
```

Code migration and automated verification are complete. User acceptance in the
native GUI terminal covers font/IME behavior, drag selection, clipboard and
trackpad feel; pyte is not a substitute for those device-level checks.

Markdown file and web links emit validated OSC 8 hyperlinks; relative file targets resolve against the session workspace. Because the transcript enables mouse reporting, the terminal never sees a plain click on a link, so the TUI opens the destination itself (the platform opener, with `file://` targets passed as paths) and reports a failure as a notice; Cmd-click still works where the terminal supports it. Unknown/default reasoning effort is omitted from the footer, and resolved decisions clear their notice on authoritative snapshots.

The managed transcript enables mouse reporting. Each child row in a Task group opens that exact session; the group heading opens the agent list. Esc returns from a child to its parent. Native terminal text selection uses the terminal mouse override (usually Shift-drag).

All transient surfaces (tool/thought details, model/permission/effort selectors, approvals, questions, history search, tasks, context and completion menus) overlay the base frame. They do not reserve transcript rows or change its scroll offset. Focus-owning dialogs may cover the composer; closing reveals its original position and draft. Tool/thought details start at the clicked transcript record and cover subsequent rows in place; they scroll independently, and popup wheel events never scroll the underlying transcript. The full transcript reader remains an explicit separate screen.

### Session cache indicator

The footer shows `cache N%` beside context usage. This is the token-weighted
cache read ratio across requests owned by the viewed session, excluding child
usage folded into billing totals. It survives resume and compaction. The context
popover shows the numerator/denominator and scope. `cache —` means no input usage;
`cache ?` means missing/partial provider reporting or legacy history without
cache-presence metadata. An explicit cached-token count of zero shows `cache 0%`.
