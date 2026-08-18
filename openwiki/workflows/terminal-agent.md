---
type: Engineering Workflow
title: Terminal coding-agent runtime and safety workflow
description: Maintainer guide to the terminal UI, headless mode, ACP service, sessions, approvals, tools, and browser bridge.
tags: [terminal, approvals, sessions, acp, security]
---

# Terminal coding-agent runtime and safety workflow

## Entry and construction

`cmd/dacode` delegates to `internal/dacode.Run`. `Run` parses flags, resolves the
working and state directories, selects authentication/model settings, applies startup
automation, then chooses one of three surfaces:

```text
interactive -> newRunner -> Bubble Tea model -> streamed turns and local modals
headless    -> newRunner -> bounded non-interactive loop -> text or versioned JSON
ACP         -> daacp server -> per-session runner factory -> editor-owned session UI
```

`runner.go` assembles the coding profile, workspace/local context, memory, filesystem,
shell policy, goals, subagents, approval middleware, checkpoint saver, and model
middleware. UI policy must not be the only enforcement point: delegated agents and
direct backend calls need the same execution and filesystem boundaries.

## Sessions and durable state

SQLite checkpoints own thread transcripts and private application fields. New and
resumed threads preserve their original working directory, selected agent/model, goal,
memory identity, and other explicitly versioned state. Invalid persisted values fail
closed instead of silently widening authority. Session selectors and direct resume must
restore transcript and usage before accepting follow-up work.

ACP `session/load` separately verifies the persisted original directory and replays
human, assistant, tool-call, and tool-result updates with replay metadata. Dynamic ACP
model changes rebuild a session runner without discarding its thread history and persist
the effective model for later load.

## Approval modes

Manual pauses gated actions for the user. Auto uses a separate read-only reviewer and
returns uncertain or failed review to the user. YOLO resolves gates without review, but
its first entry remains inactive until the current local warning policy is persisted.
The first Auto enable shows a versioned education notice. Per-thread live policy must be
validated and enforced server-side; missing or corrupt state becomes Manual.

Approvals do not sandbox local execution. `LocalShell` can leave the rooted filesystem,
so allow lists and prompts are policy layers only. Use a remote sandbox boundary for
untrusted work. MCP tools require coherent read-only annotations to bypass headless
review; server-supplied annotations cannot protect against a malicious server.

## Terminal rendering and browser bridge

The Bubble Tea model owns transcript lifecycle, streaming, approval widgets, selectors,
status, clipboard/browser control sequences, and slash commands. All external text is
made terminal-safe before rendering; clipboard payloads preserve exact source text but
travel through bounded encoded control sequences. The xterm.js bridge handles only the
explicit browser control channels.

Every user-visible terminal behavior needs both intended-state Go tests and a Playwright
path in `internal/dacode/xtermjs/e2e`. Keep the shared browser verification gate open
while UI work is incomplete, then run the full suite—not only a grep-selected test.

## Safe modification sequence

1. Identify the owner: CLI parsing, runner construction, graph middleware, backend, persisted state, TUI state, or browser bridge.
2. Add the enforcement at the lowest boundary that all main/delegated paths share.
3. Add bounded failure and cancellation behavior before the success UI.
4. Add Go tests for state and adversarial inputs; add Playwright for every visible path.
5. Run `go test -race ./internal/dacode`, `go vet ./internal/dacode`, and `make dacode-e2e` before the root gate.
