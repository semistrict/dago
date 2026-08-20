---
type: Architecture Overview
title: dago runtime and package architecture
description: How public configuration composes middleware, tools, backends, state, subagents, and the internal graph runtime.
tags: [architecture, runtime, middleware, persistence, adapters]
---

# Runtime and package architecture

## Dependency direction

The normative dependency direction is recorded in `docs/ARCHITECTURE.md`:

```text
messages/models/tools/state
  -> stores/caches/checkpoints
  -> internal graph runtime
  -> generic agent API
  -> deep middleware and backends
  -> provider and remote adapters
  -> applications and examples
```

Core packages use provider-neutral contracts. Concrete HTTP clients, database drivers,
remote sandbox transports, credentials, and UI state remain at adapter/application
boundaries.

## Construction and execution path

1. An application calls `dago.New(model, options...)`. The model is a required positional dependency; static invalid configuration panics at construction.
2. `dago.newAgent` resolves profiles, supplies the useful in-memory backend default, builds filesystem/interpreter/todo/skills/subagent/summarization/memory/approval middleware, and applies caller middleware.
3. `dagent.New` compiles that provider-neutral configuration into the internal graph. Public callers receive `dagent.Agent`, not the graph implementation.
4. `Invoke` or `Stream` supplies messages, thread configuration, resumptions, dependencies, and configurable run context. The runtime schedules model/tool nodes, applies middleware, emits events, and observes recursion/concurrency limits.
5. A configured checkpoint saver persists thread state and interrupts. A `dastore.Store` is cross-thread application storage; it is not interchangeable with checkpoint history.

## Middleware, tools, and backends

- Tools act only when selected by the model. Middleware can alter model requests, tool calls, state, prompts, or execution around those calls.
- `dabackend.Backend` owns file operations. Execution exists only on backends that explicitly implement it. Rooted local file operations and arbitrary subprocess execution have different confinement guarantees.
- Approval middleware can pause gated tools, but approval is not sandboxing. The backend or remote service remains the containment boundary.
- Profiles add provider/harness prompt, middleware, tool-description, and exclusion behavior. Registries are additive; explicit caller configuration retains precedence.
- Main, declarative, general-purpose, runnable, and asynchronous subagents have distinct construction/inheritance boundaries. When behavior differs in delegated work, inspect which options, backend, memory, state fields, and middleware that subagent actually inherits.

## State and persistence

`dastate` fields define reducers and cloning. `dacheckpoint` stores graph checkpoints,
pending writes, metadata, and namespaces. `dagent.State`, `History`, `UpdateState`,
`Replay`, `Fork`, and `DeleteThread` are the public durable-state surface.

Private runtime fields must not leak through public state snapshots or model-visible
state. Application metadata that must survive restarts—working directory, selected
agent/model, approval mode, or UI preferences—needs an explicit versioned owner and
fail-closed validation. See the [terminal workflow](../workflows/terminal-agent.md) for
the application-specific session rules.

## Adapter boundaries

| Adapter | Boundary |
| --- | --- |
| `daproviders/*` | Converts provider APIs into `damodel.Chat`; owns credentials, HTTP behavior, and provider response validation. |
| `dabackend/*` | Converts local or remote filesystem/execution services into bounded backend contracts; transports are caller-authenticated unless documented otherwise. |
| `daacp` | Projects agent streams, tools, approvals, configuration, and durable session replay into ACP v1. |
| `daserver` | Exposes the local development API around caller-supplied agent factories and persistence. |
| `browser/*` | Supplies browser/WASM bridges, storage, checkpointing, and constrained execution variants. |
| `daeval/*` | Observes runs and produces bounded deterministic reports without granting model or provider authority. |

## Public change checklist

- Read `docs/ARCHITECTURE.md`, `docs/UPSTREAM.md`, and the affected package tests.
- Keep required inputs positional and optional defaults useful.
- Define cancellation, bounds, stable errors, JSON representation, and secret handling.
- Add intended-behavior and race coverage at the owning boundary.
- Update `docs/COMPATIBILITY.md` and `docs/SECURITY.md` when behavior or trust changes.
- Run generators before `make check`; never patch generated outputs directly.
