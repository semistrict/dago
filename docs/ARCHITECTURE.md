# Architecture and public API policy

dago is a focused Deep Agents implementation, not a general port of LangChain or
LangGraph. The dependency direction is:

`damessage/damodel/datool/dastate` → `dastore/dacache/dacheckpoint` → internal graph
→ generic agent → deep middleware and backends → provider and remote adapters →
examples.

Core contracts contain no provider SDK types. Database drivers live in concrete
subpackages. The graph runtime remains internal until it has a separately reviewed
public use case; public consumers use `dagent.Agent`.

Public construction follows one rule: a constructor exists only when it establishes
an invariant or compiles configuration into behavior. Passive configuration is an
exported value. Agent-owned facilities such as `Filesystem`, `Skills`, `Memory`, and
`Summarization` are nested on `dago.Options`; `dago.New` binds them to the agent model
and backend. Mandatory static dependencies are positional constructor parameters,
option zero values select useful defaults, and static programmer mistakes panic.
Errors are reserved for external configuration, model or tool input, I/O, remote
operations, and runtime execution where the caller can make a meaningful decision.

Public additions require intended-behavior tests, cancellation semantics for any
blocking operation, a stable JSON representation for persisted or streamed data,
and a documented compatibility row. Errors wrap stable sentinel errors where a
caller can reasonably recover. Secret values are never included in error text.

The modules target Go 1.26. For optional scalar fields that genuinely need
unset-versus-zero semantics, use `new(expr)` at construction sites instead of
adding one-off pointer helpers. Use `omitzero` for zero-value omission, especially
for `time.Time`; retain pointers only for genuine tri-state contracts. Timer-driven
tests should use `damodel/modeltest.TestWithFakeTime` when they can remain entirely
inside a `testing/synctest` bubble.

Persistent envelopes and stream records begin at version 1. Additive fields are
allowed within a major module version. Removing or reinterpreting a field requires a
new envelope version and a migration. Unknown versions fail explicitly.

The test suite distinguishes normative tests, which assert behavior anchored in the
upstream manifest, from exploratory reproductions. Reproductions are not kept in the
passing suite unless their assertion has been flipped to the intended behavior.
