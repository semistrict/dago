# Terminal command interactions

The terminal application exposes one canonical registry of 40 public slash
commands. Aliases are resolved to their canonical command before dispatch, and
urgent commands bypass active work only in their exact bare form. In
particular, arguments cannot turn `/quit`, `/restart`, or `/force-clear` into a
new urgent operation.

`/clear` waits for active work, creates a new thread, and resets thread-owned
transcript, approval, goal, rubric, usage, cost, queue, and input state. When a
completed checkpoint is known, the previous thread remains addressable through
`/threads -r ID`. `/force-clear` first fences the old operation generation, then
cancels active work and discards queued prompts and deferred changes. The old
stream is drained after it observes cancellation before durable graph cleanup is
requested, respecting the runner's after-invocation cancellation contract. Late
stream, shell, goal, rubric, selector, and runtime completions from the old
generation cannot enter the new thread.

Model, thread, agent, and MCP reconnect choices made during active work are
stored as bounded immutable deferred actions. Replacing the same action kind is
last-write-wins and moves the replacement to the end of the queue. Asynchronous
thread, agent, model-preference, and MCP changes form barriers: each completion
is observed before the next change and then the queued prompt. A failed action
does not suppress later actions or that prompt. Model-default writes are
generation-bound and roll back optimistic UI state on failure.

The thread selector normalizes and bounds checkpoint metadata before search or
rendering. Creation time comes from the first durable checkpoint; branch is
shown as unavailable when older checkpoints do not carry it. Deletion requires
the exact selector instance, snapshot generation, thread ID, and checkpoint ID.
The runner rechecks that the checkpoint is still latest immediately before its
durable deletion. Stale or failed deletion results cannot remove a replacement
row.

## Input and confirmation safety

Escape, Ctrl+C, and Ctrl+D share a deterministic priority cascade. Destructive
clear, quit, and delete-confirmation actions use a three-second monotonic double
confirmation, with delete outranking draft clear and draft clear outranking
quit. Escape-cleared drafts have one exact Ctrl+Z restoration slot, including
paste bindings and media placeholders; drafts over 1 MiB are left untouched
rather than partially snapshotted. Ctrl+C copies a
non-secret draft without clearing it. Ctrl+D deletes one Unicode rune at the
cursor without relocating a multiline cursor and quits only at the end of input
or in the explicit confirmation cascade.

## Installation boundary

The stock catalog is a useful discovery list of integrations already compiled
into the binary. It intentionally grants no arbitrary package-manager or
network authority. A custom distribution may inject a fixed external catalog
and an authenticated installer controller. Such entries still require explicit
confirmation (or the exact `--force` form), execute only the controller's fixed
executable/argument/environment allowlist, and report that a restart is needed.
Startup-recovery bypass validates the bounded `/install` syntax but never
broadens the injected catalog.
