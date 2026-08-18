# Lifecycle hooks

Package `dahook` implements the pinned Hooks v2 contract without coupling command
execution to a terminal UI. A host loads an immutable snapshot, constructs an
`Engine`, and dispatches lifecycle `Invocation` values. Client-owned events are
`SessionStart`, `UserPromptSubmit`, `SessionEnd`, `PermissionRequest`, and
`Notification`. Server-owned events are `PreToolUse`, `PostToolUse`,
`PostToolUseFailure`, `PreCompact`, `Stop`, `SubagentStart`, and `SubagentStop`.

Configuration is loaded in project, user, then enabled-plugin order. Every matching
handler runs concurrently, while decisions reduce in that stable precedence order.
Project configuration is `{project}/.deepagents/hooks.json`; user configuration is
`~/.deepagents/hooks.json`. A project source is loaded only after an explicit session
grant or a persisted interactive grant. Headless hosts ignore persisted grants and
must set `TrustProject` for that invocation. The UI that asks the operator is host
owned; selecting “always” calls `TrustProject`, which records the canonical workspace
in a private, versioned `hooks_trust.json` file. Selecting “once” passes
`TrustProject: true` without writing the store, and denial leaves it false.

The v2 `hooks` object maps event names to matcher groups. A handler may use `command`
for shell syntax or `argv` for direct argument execution. `async: true`, empty
executables, negative timeouts, unknown events, oversized files, and non-regular
configuration fail closed. Compatible list-shaped documents are migrated for their
equivalent lifecycle events; legacy tool and permission entries are intentionally not
reinterpreted because their semantics differ. Matchers select tool name, lifecycle
cause, notification type, compaction trigger, or subagent name as appropriate.

Commands receive the compatible flattened JSON object on standard input. Plain
standard output becomes additional context only for `SessionStart` and
`UserPromptSubmit`; other events require JSON. Exit status 2 maps to the event's block,
deny, feedback, or continuation behavior. JSON supports `continue`, `stopReason`,
`systemMessage`, `permissionDecision`, `permissionDecisionReason`,
`additionalContext`, and their compatible `hookSpecificOutput` forms. The default
command timeout is ten minutes, except `UserPromptSubmit`, which defaults to 30
seconds. Output is capped at 100,000 bytes per stream by default.

Server-owned events use a per-session `Capability`, `Server`, `Fulfiller`, and
`Ledger`. The host must deliver at least 32 bytes of random capability key material to
the client over its already-authenticated session channel; the key is never part of
the interrupt. Requests and responses are protocol version 1, carry snapshot and
invocation identities, and have a deadline. Every request field is covered by an
HMAC-SHA-256 capability tag. The complete response decision and its request identity
are covered by a second tag. The client atomically claims an invocation before hook
side effects and retains a bounded recent replay window without evicting in-progress
claims; the server authenticates a response before consuming its exact outstanding
request. Both sides reject stale, mismatched, concurrent, or recent duplicate
fulfillment. Hosts should persist graph interrupt/resume values through
their normal checkpoint path and create a fresh capability for each authenticated
client session.

`TranscriptStore` materializes version-1 per-thread and per-subagent JSONL projections
for `transcript_path` and `agent_transcript_path`. Writes are atomic, owner-only,
bounded, path-confined, content-revisioned, cancellable, and redact credential-shaped
keys and text, authentication and cookie headers, URL user information and credential
query parameters, and PEM private-key blocks. A projection is a stable snapshot and
may lag live graph state; Stop and SubagentStop callers should also include their
current `last_assistant_message`.

An optional engine progress callback reports bounded handler start/completion pairs.
The coding-agent status presenter coalesces updates without blocking hook execution;
when hooks overlap, the most recently started active handler wins and completion
restores the preceding active status. Tool lifecycle payloads use one shared builder:
arguments are always objects, unknown result statuses fail closed to `error`, NULs are
removed, and tool output is truncated to 2,000 Unicode code points with an explicit
marker before it reaches a hook process.
