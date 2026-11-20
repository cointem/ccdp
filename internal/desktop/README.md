# Native desktop client

The macOS client uses MyGo v0.9.2's native Go UI. It has no web server,
JavaScript bridge, browser dependencies or frontend build. `mygo.json` packages
`cmd/ccdp-desktop` independently from `cmd/ccdp`. MyGo requires Go 1.27.3+.

## Ownership

- `host.go` owns concrete Agent construction, resume and shutdown. An opened
  session stays alive across navigation, and each saved log has one handle.
  Runtime creation and shutdown run outside the UI thread.
- `app.go` connects each conversation to `protocol.SessionClient`. Watch
  reconnects from an authoritative snapshot without replaying commands. An
  uncertain input retains its command and input IDs for an explicit retry.
  Each conversation owns its draft, projection, scroll and decision state.
- `projection.go` consumes cumulative, identified transcript items. A committed
  item's `PreviousID` replaces its live identity. Snapshots and state updates
  replace the transcript window and clear resolved decisions. Older pages come
  from `SessionDirectory.ReadTranscript`, never from message-history rendering.
- `view.go` renders the sidebar, virtual transcript list, Markdown, composer,
  approval controls and question form. All application state changes reach the
  main thread through `Window.Update`. Native elements are build-local values.
- `cmd/ccdp-desktop/main.go` loads the same configuration as the CLI and dispatches
  code-mode subprocess workers before starting the UI.

Closing the app cancels observers, then joins the runtime's cleanup before
quitting. The project sidebar lists user chats and their forks. The right
panel shows child agents, their reports and pending decisions alongside the
parent chat. Selecting a file link, a successful file operation, or an agent
opens that item in a closable tab. The plus button can open a project file or
a child agent. Closing the last tab collapses the panel.

## Development

```sh
go run ./cmd/ccdp-desktop -d /path/to/project
go test -race ./internal/desktop
go vet ./internal/desktop ./cmd/ccdp-desktop
sh scripts/build-macos.sh
```

The build script fetches the app icon from the ccdp v0.2.0 release and the host platform's `rg` executable from the
[ripgrep 15.2.0 release](https://github.com/BurntSushi/ripgrep/releases/tag/15.2.0).
Their Apple Darwin release archives have the following SHA-256 digests.

| Architecture | Archive SHA-256 |
| --- | --- |
| aarch64 | `3750b2e93f37e0c692657da574d7019a101c0084da05a790c83fd335bad973e4` |
| x86_64 | `af7825fcc69a2afc7a7aea55fc9af90e26421d8f20fe59df32e233c0b8a231c1` |

Run this integration test from a macOS host terminal with Seatbelt and local
HTTP listening available. It uses a local model fixture, temporary project,
runtime and archive directories; no real provider credentials are needed:

```sh
CCDP_DESKTOP_INTEGRATION=1 go test -race ./internal/desktop -count=1
```

To render repeatable UI fixtures for visual inspection:

```sh
CCDP_DESKTOP_SCREENSHOTS=/tmp/ccdp-desktop-preview \
  go test ./internal/desktop -run TestDesktopLayout -count=1
```

The screenshots contain fixture conversations, not the user's saved data. The
interaction tests exercise multiline Chinese text, sending, one-time approval,
stable retry identity, stream finalization and minimum window dimensions.

## Current scope

The client supports project selection, saved-session search/resume, background
sessions, streamed Markdown with highlighted code, paginated conversation
history, bounded tool previews, model/permission changes, stopping, approvals,
questions and child conversation navigation. Light/dark appearance follows the
system. The Settings page edits providers, credentials, model limits, default
models and permissions in the same configuration file as the CLI. Saving updates
drafts and refreshes open root sessions at the next request boundary. An open
chat keeps its selected model while it remains available.

The project menu stores display names, pins, and collapsed groups in
`~/.ccdp/desktop-projects.json`. Project instructions are edited in their
workspace files. The left navigation rail opens chats, customization and
settings. The Plugins page lists installed plugin bundles and configured MCP servers, including Exa before a session connects. The dedicated MCP page, also linked from Settings, adds personal connections and edits or removes a registration in its owning configuration file. New sessions use the updated server list. The Skills page imports individual skills into personal or project scope. The project's Extensions tab lists configuration sources and the MCP
tools published by a connected session in that project.

Image input, slash-command discovery, Git diff review, goal management controls,
desktop computer-use opt-in, notifications, keychain onboarding and signed
distribution are future work. Tool output previews explicitly identify
truncation; full output and advanced commands remain available in the CLI.
