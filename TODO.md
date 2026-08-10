# Deep Agents Go Port Plan

## Purpose

Build an idiomatic Go implementation of the Deep Agents SDK, together with the
smallest LangGraph and LangChain-compatible substrate required by that SDK.
The goal is behavioral parity at the public Deep Agents boundary, not a line-by-line
translation and not a general-purpose port of the full upstream frameworks.

This file is the living implementation record. The dependency cut and phase order
remain useful for future upstream refreshes; checked items are implemented and covered
by the evidence summarized in `docs/COMPATIBILITY.md`.

## Implementation status

The core port is implemented: public Deep Agent construction, the focused agent and
graph substrate, mandatory delta channels, state/filesystem/store/composite/shell,
LangSmith sandbox, and Context Hub backends, SQLite and PostgreSQL savers, safe
cross-language checkpoint fixtures, middleware features, OpenAI access, examples,
and release gates.

The following integrations were deliberately evaluated and deferred because they are
not required by the pinned local Deep Agents contract: a public general-purpose graph
API, parent-graph routing, hosted asynchronous subagents, a remote graph client, video
decoding, general tracing/deployment integrations, ACP, and a general-purpose CLI.
The Shelley application is included as a complete example rather than a supported
hosted service.

## Source baseline

The plan was prepared against these local source revisions:

| Source | Local path | Revision | Role |
|---|---|---:|---|
| Deep Agents Python | `/Users/ramon/src/deepagents` | `d60560d695e8c436e11dee96965e7a1447409737` | Normative product behavior and public feature set |
| LangGraph Python | `/Users/ramon/src/langgraph` | `fde3068970679184b68d3d068a92c83c966a4888` | Normative graph, state, persistence, interrupt, and streaming semantics |
| LangChain Python | `/Users/ramon/src/langchain-py` | `d048fbe170573b6e7056b5ef5f78d8451e54abaf` | Normative agent factory, middleware, message, model, and tool behavior |
| LangChain TypeScript | `/Users/ramon/src/langchain` | `62fc484b2a0d1ec5b8bebff4a8a0efe6300ada72` | Cross-language reference for messages, tools, model calls, agent nodes, and middleware |
| Deep Agents TypeScript | `/Users/ramon/src/deepagentsjs` | `945b362d06d03728d16bc0020cb242a9eeae8451` | Cross-language reference for an existing non-Python implementation |
| Existing Go experiment | `/Users/ramon/src/deepagents-go` | unversioned local tree | Reference only; mine useful tests and lessons, but do not treat as the new port's base |
| Target | `/Users/ramon/src/dago` | initialized on `main` | Go implementation |

### Python LangChain scope anchor

`/Users/ramon/src/langchain` is the TypeScript repository. The Python repository is
now separately pinned at `/Users/ramon/src/langchain-py`. At that revision, the
active `langchain` package is under `libs/langchain_v1`, its version is `1.3.14`, and
it requires `langchain-core >=1.5.3,<2` and `langgraph >=1.2.5,<1.3`. The legacy
package under `libs/langchain` is not part of this port unless the import inventory
finds an unavoidable dependency.

Use the Python implementation as the normative LangChain reference. Use the
TypeScript implementation to clarify language-neutral design choices, never to
silently override differing Python behavior.

## Normative source hierarchy

When sources disagree, resolve behavior in this order:

1. Public Deep Agents documentation and public Python tests at the pinned revision.
2. Public Deep Agents Python implementation at the pinned revision.
3. Public Python LangChain and LangGraph tests for the exact dependency versions.
4. TypeScript Deep Agents behavior where it intentionally mirrors Python.
5. TypeScript LangChain behavior where it clarifies a language-neutral contract.
6. Existing Go experiments only as design input, never as proof of parity.

Record every deliberate incompatibility in a compatibility document with rationale
and a migration note.

## Project outcomes

The completed project should provide:

- A provider-neutral Go API for constructing and running a deep agent.
- Tool-calling messages, model requests and responses, structured tool schemas, and
  streaming events.
- An agent loop with ordered middleware hooks, dynamic tools and prompts, state
  updates, tool execution, cancellation, and deterministic error handling.
- The graph/runtime semantics Deep Agents actually needs: reducers, commands,
  required delta channels, conditional routing, parallel tool dispatch, checkpointed
  execution, interrupts, resume, store access, caching, and streaming.
- Pluggable state, filesystem, composite, store, local-shell, and sandbox backends.
- Built-in filesystem tools with the same path, permission, pagination, truncation,
  and large-output behavior expected by Deep Agents.
- Inline subagents, context summarization, skills, memory, human approval, and
  profile-driven customization.
- A conformance suite that validates public behavior against versioned fixtures
  derived from upstream tests.
- Clear boundaries for optional provider integrations, remote execution, tracing,
  evaluation, and deployment tooling.

## Non-goals

- Porting all of LangChain, including chains, retrieval, document loaders, vector
  stores, or every provider integration.
- Porting all of LangGraph, including every channel type, visualization feature,
  remote client, deployment server, or database adapter.
- Reproducing Python's dynamic typing, decorators, or class hierarchy in Go.
- Source-level or package-name compatibility with Python.
- Reproducing Python-only checkpoint payloads such as pickle data, arbitrary Python
  constructors, Pydantic reconstruction, NumPy values, or import-and-call behavior.
- Claiming universal payload compatibility for checkpoint state containing arbitrary
  user-defined Python objects.
- Shipping a general-purpose CLI, ACP server, or hosted service as part of SDK parity.
  The Shelley web application is maintained as an end-to-end example.
- Enabling shell execution by default.
- Preserving accidental behavior or known bugs merely because a source test exposes
  them. Tests must encode intended correct behavior.

## Porting principles

- Port contracts and behavior, not syntax.
- Keep one context-aware Go execution API instead of duplicating Python sync and
  async APIs.
- Prefer small interfaces accepted by consumers over large inheritance-shaped
  interfaces.
- Keep Deep Agents public APIs separate from internal framework compatibility code.
- Start internal packages private; promote them only when a stable external use case
  is proven.
- Make state changes explicit and reducer-driven. Avoid shared mutable state hidden
  behind middleware.
- Make cancellation, goroutine ownership, backpressure, and cleanup part of every
  streaming and concurrent API contract.
- Make persistence envelopes versioned from the first persistent implementation.
- Keep checkpoint schema compatibility, safe payload interoperability, and runtime
  state compatibility as separately testable claims.
- Treat tools and backends as authority boundaries. Prompts are not a security
  boundary.
- Keep provider-specific request fields behind capability interfaces rather than in
  the core agent loop.
- Preserve upstream notices for substantively copied code and fixture data.

## Required dependency cut

### Port from LangChain core

Only the surface used by the agent factory and Deep Agents should be implemented:

- Canonical message roles and content blocks: human, assistant, system, tool,
  removal, text, image/file references, tool calls, tool results, usage, and metadata.
- Message identity, serialization, copying, replacement, removal, and the message
  reducer behavior required by checkpoints and compaction.
- Tool definitions with stable names, descriptions, JSON Schema input, execution,
  typed runtime injection, and result normalization.
- A provider-neutral chat-model interface supporting tool binding, model profiles,
  token usage, streaming deltas, structured output capabilities, and optional token
  counting.
- Runnable configuration concepts actually consumed by Deep Agents: context,
  configurable values, metadata, tags, recursion limits, callbacks/events, store,
  and cancellation.
- Agent state, model request/response, tool-call request, middleware contracts, and
  structured-output strategies.
- The middleware required by the default and built-in profile paths: human approval,
  tool retry, todo-list support, and provider prompt-caching hooks.

Do not port unrelated LangChain modules.

The Python source inventory narrows that surface to:

| Capability | Normative Python source | Disposition |
|---|---|---|
| Agent graph construction and routing | `libs/langchain_v1/langchain/agents/factory.py` | Port behavior behind Go-native APIs |
| Middleware lifecycle, state, model/tool requests | `libs/langchain_v1/langchain/agents/middleware/types.py` | Port the contracts; collapse sync/async into context-aware Go APIs |
| Structured response strategies and errors | `libs/langchain_v1/langchain/agents/structured_output.py` | Port the strategies and observable error behavior |
| Human approval, todo, retry | `libs/langchain_v1/langchain/agents/middleware/human_in_the_loop.py`, `todo.py`, `tool_retry.py` | Port the subset used by Deep Agents |
| Summarization shared types/constants | `libs/langchain_v1/langchain/agents/middleware/summarization.py` | Port only dependencies required by Deep Agents context management |
| Chat-model resolution and capability surface | `libs/langchain_v1/langchain/chat_models/base.py` | Adapt to provider-neutral Go interfaces; keep provider loading outside core |
| Messages and content | `libs/core/langchain_core/messages` | Port the language-neutral data model, reducers, conversion, and token approximation used by Deep Agents |
| Model contracts and profiles | `libs/core/langchain_core/language_models` | Port minimal chat-model and capability contracts |
| Tool contracts and structured tools | `libs/core/langchain_core/tools` | Port execution, schemas, runtime injection, and normalization |
| Runnable configuration | `libs/core/langchain_core/runnables/config.py` | Port only configuration semantics consumed by the agent runtime |
| Compiled runnable subagents | Runnable protocol used by `langchain.agents` and Deep Agents | Define a small local invocation/streaming interface |

Do not port Python deprecation or beta decorators, arbitrary runnable composition,
provider implementations, private provider conversion helpers, tracing, or the remote
graph client into the core. Provider adapters and remote asynchronous subagents remain
separate optional work.

### Port from LangGraph

Implement a deliberately narrow internal runtime sufficient for the agent factory:

- State keys with per-key reducers and deterministic update application.
- Last-value, binary-operator, and ephemeral behavior required by agent state.
- Delta channels as a required first-class channel type, including delta updates,
  snapshot seeds, version tracking, history reads, reconstruction, and compaction.
- Nodes, normal edges, conditional edges, start/end markers, and graph compilation.
- Commands carrying update, goto, resume, and parent-graph intent.
- Sends for parallel tool work and deterministic result aggregation.
- A superstep execution loop with retry boundaries, recursion/step limits,
  cancellation, and no goroutine leaks.
- Runtime context, store, stream writer, previous state, and tool-call identity.
- Checkpoint saver contracts, in-memory implementation, pending writes, thread and
  checkpoint namespaces, replay, fork, pause, and resume.
- Namespaced store contracts and an in-memory implementation with batch operations.
- Cache contracts and an in-memory implementation.
- Interrupt and resume semantics required by human approval.
- Invoke and streaming modes needed by agent consumers.

Do not expose this as a complete public LangGraph implementation until independent
use cases and conformance coverage justify that promise.

### Python-compatible SQLite and PostgreSQL checkpointers

The standard SQLite and PostgreSQL savers are required. Compatibility has two
separate levels and neither may be used as shorthand for the other:

1. **Schema and saver compatibility is required.** Go must use the same table names,
   columns, keys, migration numbering, ordering rules, conflict behavior, namespaces,
   checkpoint identifiers, pending-write semantics, and delta reconstruction rules as
   the pinned Python savers.
2. **Payload interoperability is required only for the language-neutral safe subset.**
   Scalars, nulls, bytes, lists, string-keyed maps, checkpoint envelopes, metadata,
   channel versions, and explicitly specified agent message/tool/state records must
   have cross-language fixtures before being called interoperable.

Do not implement pickle, arbitrary Python module/class constructor tags, Pydantic or
NumPy reconstruction, dynamic imports, or execution of serialized callables. An
unsupported Python-specific value must produce a typed compatibility error identifying
the checkpoint, channel, and encoding tag. It must never be guessed, dropped, or
executed. If Python needs to read a Go checkpoint containing richer Go values, those
values must first be represented by an explicitly versioned language-neutral record
that has a Python reader; sharing a database schema alone is not evidence that either
runtime can resume the other's arbitrary state.

In particular, the default Python serializer's tagged reconstruction of message,
command, send, interrupt, UUID, datetime, Pydantic, and NumPy classes is not itself a
Go porting target. A concept such as a message may still be interoperable when both
runtimes encode it as an approved plain-data record; the Python module/class tag is
not. Cross-language fixtures must state which serializer and representation produced
them.

Use these pinned sources as the storage specification:

- Core model, IDs, metadata, pending writes, and serializer boundaries:
  `libs/checkpoint/langgraph/checkpoint/base` and
  `libs/checkpoint/langgraph/checkpoint/serde`.
- SQLite saver and delta reconstruction:
  `libs/checkpoint-sqlite/langgraph/checkpoint/sqlite`.
- PostgreSQL saver and migrations:
  `libs/checkpoint-postgres/langgraph/checkpoint/postgres`.
- Cross-saver behavior:
  `libs/checkpoint-conformance/langgraph/checkpoint/conformance/spec`.

The SQLite schema contract is:

- `checkpoints`: `thread_id TEXT NOT NULL`,
  `checkpoint_ns TEXT NOT NULL DEFAULT ''`, `checkpoint_id TEXT NOT NULL`, nullable
  `parent_checkpoint_id TEXT` and `type TEXT`, nullable `checkpoint BLOB` and
  `metadata BLOB`, primary key `(thread_id, checkpoint_ns, checkpoint_id)`.
- `writes`: `thread_id TEXT NOT NULL`,
  `checkpoint_ns TEXT NOT NULL DEFAULT ''`, `checkpoint_id TEXT NOT NULL`,
  `task_id TEXT NOT NULL`, `idx INTEGER NOT NULL`, `channel TEXT NOT NULL`, nullable
  `type TEXT` and `value BLOB`, primary key
  `(thread_id, checkpoint_ns, checkpoint_id, task_id, idx)`.
- WAL mode, lexicographic checkpoint ordering, and Python's replace-versus-ignore
  behavior are part of compatibility. The pinned SQLite saver has no checkpoint
  migration table and does not persist `task_path` at this revision.

The PostgreSQL contract includes the same migration history and final schema as the
pinned standard saver:

- `checkpoint_migrations`: `v INTEGER PRIMARY KEY`, with migrations applied and
  recorded in upstream order.
- `checkpoints`: non-null thread, namespace, and checkpoint ID text columns; nullable
  parent and type text columns; `checkpoint JSONB NOT NULL`; and
  `metadata JSONB NOT NULL DEFAULT '{}'`; primary key
  `(thread_id, checkpoint_ns, checkpoint_id)`.
- `checkpoint_blobs`: non-null thread, namespace, channel, version, and type text
  columns plus nullable `blob BYTEA`; primary key
  `(thread_id, checkpoint_ns, channel, version)`.
- `checkpoint_writes`: non-null thread, namespace, checkpoint ID, task ID, integer
  index, channel, and type columns; `blob BYTEA NOT NULL`; and
  `task_path TEXT NOT NULL DEFAULT ''`; primary key
  `(thread_id, checkpoint_ns, checkpoint_id, task_id, idx)`.
- Preserve migrations 0 through 9, including the nullable blob change, intentional
  no-op, concurrent indexes, and `task_path` addition. Do not collapse the history
  into a cleaner Go-only migration.

Both savers must preserve the upstream special write indexes, UUID ordering,
pending-send order, idempotence, parent traversal, and snapshot-aware pruning. The
PostgreSQL shallow saver, graph stores, caches, and vector stores are deferred and are
not implied by this requirement.

Delta snapshots are a required language-neutral graph record, not a Python-specific
object. Go must read and write the pinned snapshot representation, including its
MessagePack extension identifier `7`, seed-versus-write distinction, parent-chain
reconstruction, oldest-to-newest application order, and the PostgreSQL rule that a
blob snapshot takes precedence over its inline marker. A saver without delta-channel
support is non-conforming.

### Port from Deep Agents

The primary Python SDK under `libs/deepagents/deepagents` is the product scope:

- Graph assembly and option resolution from `graph.py`.
- Backend contracts and result types from `backends/protocol.py`.
- State, filesystem, store, composite, local-shell, and sandbox backend behavior.
- Filesystem middleware, permissions, file tools, execution, result truncation, and
  result offloading.
- Inline subagents and the task tool.
- Summarization, explicit compaction, argument truncation, history offloading, and
  media reference handling.
- Skills and memory loading and prompt injection.
- Tool-call patching, message eviction, tool exclusion, and prompt-caching behavior.
- Human approval integration and filesystem-derived approval rules.
- Harness/provider profile registration, merge, exclusion, and override rules.
- Public structured response behavior and rubric middleware.

Remote asynchronous subagents, video processing, vendor-hosted backends, tracing
integrations, and provider-specific profile shims should remain optional work after
the core parity gate.

## Proposed architecture

Keep the initial public surface small. The exact package names are a design output of
Phase 1, but responsibilities should remain separated as follows:

| Layer | Responsibility | Visibility |
|---|---|---|
| Public SDK | Agent construction, options, invocation, streaming, public backend and middleware extension points | Public |
| Message/model/tool contracts | Stable provider-neutral data model and minimal capability interfaces | Public only where users need to implement adapters |
| Agent factory | Model/tool loop, routing, structured output, middleware ordering, dynamic tools | Internal initially |
| Graph runtime | Reducers, commands, sends, execution, checkpoints, interrupts, cache, store | Internal initially |
| Deep middleware | Filesystem, subagents, summarization, skills, memory, approval, profiles | Public extension points plus internal helpers |
| Backends | State, filesystem, composite, store, shell, sandbox, optional remote adapters | Public interfaces; concrete implementations split by dependency |
| Provider adapters | Model-specific message translation, streaming, tool and structured-output capabilities | Separate packages |
| Conformance | Shared backend/model/runtime fixtures and black-box parity scenarios | Internal test support |

### Key Go design decisions

- Represent public messages with explicit tagged content blocks, not unrestricted
  maps. Preserve an extension field for provider metadata that survives round trips.
- Represent agent state as a controlled key/value state bag plus a reducer registry,
  with typed helpers for built-in fields. Do not force every user state shape through
  reflection-heavy generic structs.
- Use `context.Context` for cancellation, deadlines, runtime values, and tracing
  propagation, but do not hide mutable agent state in the context.
- Return explicit state updates or commands from nodes and middleware. Reject
  ambiguous mixed return forms at boundaries.
- Model streams as owned iterators or channels with an explicit close/error contract.
  A producer must stop promptly when the consumer cancels.
- Define tool execution independently of model providers. Provider adapters translate
  the common JSON Schema and tool-call representation.
- Use functional options only for construction-time configuration. Runtime behavior
  should be visible in request values and interfaces.
- Keep the default shell capability absent. Add it only when the selected backend
  implements the sandbox execution contract.
- Avoid a dependency on the full existing Go LangChain library unless Phase 1 proves
  its message, model, tool, and streaming contracts can satisfy parity without
  adapter leakage. Prefer focused adapters over coupling the core module to its full
  API and dependency graph.

## Compatibility matrix to maintain

Create and maintain a table during implementation with one row per behavior and the
following states: `unplanned`, `planned`, `implemented`, `verified`, `deferred`, or
`intentionally different`.

At minimum, track:

- Agent construction with explicit model and custom tools.
- System prompt assembly and profile overrides.
- Default middleware order and custom middleware replacement/insertion.
- Dynamic prompt and dynamic tool visibility.
- Model tool-call loop and parallel tool calls.
- Tool results, errors, retry, direct return, and structured output.
- State reducers, delta messages, private state, context, and runtime access.
- Delta channel writes, snapshots, history, replay, migration, and snapshot-aware
  pruning in memory, SQLite, and PostgreSQL.
- Invoke, token/message/update streaming, cancellation, and recursion limits.
- Checkpoint persistence, replay, pause, resume, and fork by thread.
- SQLite schema and saver compatibility with the pinned Python implementation.
- PostgreSQL migrations, schema, and saver compatibility with the pinned Python
  implementation.
- Bidirectional checkpoint payload interoperability for the documented safe subset,
  plus typed rejection of Python-specific encodings.
- State/store/cache backend injection.
- File list, read, write, edit, delete, glob, literal grep, and execute.
- Path normalization, virtual roots, permissions, pagination, truncation, and large
  result offloading.
- Composite routing and route-specific permissions.
- Inline subagent delegation and isolated message context.
- Summarization triggers, explicit compaction, history offloading, and tool-argument
  truncation.
- Skills discovery/validation/loading and memory prompt injection.
- Human approve/edit/reject flow.
- Harness profiles, tool descriptions, middleware/tool exclusion, and prompt cache
  placement.
- Rubric/structured grading behavior.
- Optional remote asynchronous subagents and provider-specific extras.

## Port phases

The phases are dependency ordered. A later phase must not compensate for a failed
earlier contract by adding special cases.

### Phase 0 — Freeze the specification and provenance

- [x] Clone and pin Python LangChain separately from the TypeScript checkout; record
  its exact repository revision and active package versions.
- [x] Confirm the target module path, repository ownership, and minimum Go version.
- [x] Use the MIT license and a Go 1.26 support baseline on Linux and macOS; keep
  compatibility claims limited to conformance-tested surfaces.
- [x] Create an upstream manifest recording repository URLs, revisions, package
  versions, license identifiers, and the files/tests used for each ported behavior.
- [x] Preserve the completed Deep Agents import inventory for LangChain, LangChain
  Core, LangGraph, the graph SDK, provider packages, and tracing packages in the
  upstream manifest, and rerun it whenever a source revision changes.
- [x] Classify each import as `port`, `adapt`, `replace`, `optional`, or `defer`.
- [x] Create the initial compatibility matrix from Python public tests and
  documentation.
- [x] Record intended incompatibilities, especially around Python reflection,
  decorators, sync/async duplication, Python-specific checkpoint payloads, and
  private APIs.
- [x] Decide whether the existing Go experiment contributes any fixtures or API
  lessons. Copy no implementation until provenance and behavioral correctness are
  reviewed.

Exit gate: every required upstream symbol has an owner and disposition; no required
behavior depends on an unpinned or stale source tree.

### Phase 1 — Establish project and conformance foundations

- [x] Initialize the Git repository on `main` and the
  `github.com/semistrict/dago` Go module with a Go 1.26 minimum.
- [x] Define package boundaries, dependency direction, public API review rules, error
  conventions, serialization conventions, and versioning policy.
- [x] Add one-command formatting, vetting, unit tests, race tests, and coverage
  reporting.
- [x] Build test-only scripted model and recording tool doubles that can drive exact
  sequences without network access.
- [x] Define versioned JSON fixtures for messages, tool schemas, model responses,
  state updates, commands, checkpoints, and stream events.
- [x] Add fixture-generation scripts that consume upstream source tests or explicit
  source inputs. Never edit generated fixtures directly.
- [x] Add a drift check that reports changed upstream files and affected compatibility
  rows when an upstream revision is advanced.
- [x] Document which tests are normative behavior and which are exploratory
  reproductions; only correct intended behavior belongs in the normal green suite.

Exit gate: an empty vertical slice can be tested deterministically, fixtures carry
source provenance, and CI can detect races and upstream drift.

### Phase 2 — Implement messages, tools, and model contracts

- [x] Define canonical messages, content blocks, tool calls, tool results, usage,
  response metadata, IDs, and removal markers.
- [x] Implement lossless JSON serialization and stable equality rules for fixture
  comparison.
- [x] Implement message reduction, replacement, deletion, full reset, and delta
  behavior used by Deep Agent state.
- [x] Define tool metadata, JSON Schema inputs, execution, runtime injection, result
  normalization, artifacts, and errors.
- [x] Define model request/response, streaming chunks, tool binding, structured output,
  model capability profiles, and token-count hooks.
- [x] Decide whether existing Go provider libraries are used through adapters or
  replaced by focused provider clients. Keep this decision outside core contracts.
- [x] Verify provider-neutral behavior with scripted models before adding a live
  provider.

Exit gate: message and tool fixtures round-trip, scripted model streams are
deterministic and cancellable, and no provider-specific type leaks into the core API.

### Phase 3 — Implement the minimal graph runtime

- [x] Define state schemas, state keys, reducers, input/output projections, and
  validation of conflicting updates.
- [x] Implement last-value, binary-reducer, and ephemeral channels.
- [x] Implement the delta-channel core: batching-invariant reducer contract, explicit
  updates and overwrites, missing/snapshot/legacy restoration, replay from the last
  overwrite, defensive copying, snapshot cadence counters, and intended-behavior
  tests.
- [x] Integrate mandatory delta channels with version allocation, graph supersteps,
  snapshot creation, saver history queries, and compaction-safe ancestry.
- [x] Implement nodes, edges, conditional routing, start/end, graph validation, and
  compilation.
- [x] Implement commands, sends, update aggregation, and deterministic parallel result
  ordering. Parent-graph routing is intentionally deferred because the pinned Deep
  Agents execution path does not consume it.
- [x] Implement the superstep runner with recursion limits, retries, cancellation,
  panic containment, resource cleanup, and explicit task errors.
- [x] Implement runtime context, stream writer, store/cache access, and previous state.
- [x] Implement checkpoint saver interfaces and an in-memory saver, including pending
  writes, namespaces, replay, fork, interrupt, and resume.
- [x] Implement namespaced store and cache interfaces with in-memory versions.
- [x] Implement invoke and the required stream modes with bounded backpressure.
- [x] Add reducer, routing, replay, cancellation, deadlock, goroutine leak, fuzz, and
  race tests.

Exit gate: a small state graph can execute, stream, checkpoint, interrupt, resume,
fork, and parallelize deterministically under the race detector.

### Phase 4 — Implement compatible SQLite and PostgreSQL checkpointers

- [x] Freeze the pinned Python `Checkpoint`, metadata, tuple, configuration, pending
  write, special-channel, namespace, parent, UUID, and channel-version contracts.
- [x] Define a safe typed serializer interface whose support matrix distinguishes
  language-neutral values, explicitly versioned agent records, and unsupported
  Python-specific values.
- [x] Implement null, scalar, bytes, list, string-keyed map, checkpoint envelope,
  metadata, and approved agent message/tool/state record codecs without Python imports
  or object construction.
- [x] Reject pickle, arbitrary constructors, Pydantic, NumPy, callable, and unknown
  extension tags with stable typed errors and checkpoint/channel context.
- [x] Implement the standard SQLite saver with the exact upstream tables, primary
  keys, defaults, WAL setup, ordering, metadata filtering, replace/ignore write
  behavior, parent traversal, and delta reconstruction.
- [x] Implement the standard PostgreSQL saver with migrations 0 through 9 in the same
  order, the exact final tables and indexes, inline-versus-blob channel behavior,
  write conflict rules, task-path ordering, and lexical channel versions.
- [x] Port the upstream checkpoint conformance cases for put, get, list, writes,
  deletion, metadata filters, namespaces, copy, prune, delta history, and concurrency.
- [x] Generate Python-owned SQLite fixtures that Go reads and continues, and Go-owned
  fixtures that Python reads and continues, limited to the documented safe payload
  subset.
- [x] Run the same bidirectional scenarios against PostgreSQL, including databases
  migrated through representative older upstream versions rather than only fresh
  final schemas.
- [x] Compare SQLite schema introspection and PostgreSQL catalog/index/migration rows
  against Python-created databases.
- [x] Cover primitives, nested containers, bytes, approved message/tool records,
  overwrite records, delta snapshots, and pending writes in cross-language fixtures.
  Keep Python constructor-based identifiers, timestamps, commands, sends, and
  interrupts outside the shared payload claim unless a future plain-data record is
  versioned in both runtimes.
- [x] Require bidirectional delta snapshot fixtures, including the pinned MessagePack
  extension record, seed values, ordered writes, migrated pre-delta values, and
  multiple snapshot generations.
- [x] Test transactional failure, cancellation, concurrent writers, idempotent writes,
  checkpoint ordering, fork/resume, inline-versus-blob precedence, migrated plain
  values, and snapshot-aware pruning.
- [x] Publish an exact payload support table. A passing schema test must not be reported
  as cross-language resume compatibility.

Exit gate: Python and Go create indistinguishable standard SQLite/PostgreSQL schemas
and saver behavior, safely exchange every documented language-neutral payload type,
and fail explicitly on Python-specific values without corrupting the database.

### Phase 5 — Implement the LangChain-style agent factory

- [x] Define agent state, input/output state, model request/response, tool-call request,
  and runtime types.
- [x] Define the middleware lifecycle: before agent, before model, model wrapper,
  after model, tool wrapper, and after agent.
- [x] Specify and test exact middleware nesting and response ordering for both model
  and tool wrappers.
- [x] Merge middleware state fields and reducers while detecting incompatible
  definitions early.
- [x] Merge static, middleware-provided, and dynamically selected tools while
  rejecting unexecutable dynamic tools.
- [x] Build the model/tool graph with conditional routing, parallel tool dispatch,
  direct-return tools, and loop termination.
- [x] Implement structured-output strategies, validation failures, retries, and final
  structured response state.
- [x] Implement human approval middleware on top of graph interrupts and resume.
- [x] Implement only the todo and tool-retry middleware needed by Deep Agents profiles.
- [x] Validate state, middleware, tool, structured-output, streaming, and approval
  behavior against upstream fixtures.

Exit gate: the generic agent factory passes its compatibility rows without importing
Deep Agents middleware.

### Phase 6 — Implement backend contracts and conformance suites

- [x] Translate backend result types and file metadata into explicit Go structs with
  validated invariants.
- [x] Define the base backend and optional sandbox execution capability as separate
  small interfaces.
- [x] Define sync behavior only; concurrency is controlled through `context.Context`
  and implementations rather than duplicate method families.
- [x] Implement the state backend using graph state updates.
- [x] Implement the namespaced store backend using the runtime store.
- [x] Implement the host filesystem backend with an explicit root and virtual path
  mode.
- [x] Implement composite prefix routing with normalized longest-prefix selection and
  cross-route operation rules.
- [x] Implement local-shell/sandbox capability behind explicit construction and
  authority checks.
- [x] Port the backend standard tests for listing, reading, writing, editing, deleting,
  globbing, literal grep, download/upload, execution, timeout, truncation, and
  lifecycle behavior.
- [x] Add path traversal, symlink escape, permission, cancellation, large-file, binary,
  malformed encoding, and platform-separator tests.

Exit gate: every concrete backend passes the same conformance suite, and non-sandbox
backends cannot expose execution accidentally.

### Phase 7 — Assemble the first Deep Agent vertical slice

- [x] Define the public constructor and options for model, tools, system prompt,
  middleware, backend, state/context, checkpoint, store, cache, debug, and name.
- [x] Implement explicit model resolution without forcing a default provider.
- [x] Assemble the required middleware stack in the same semantic order as Python.
- [x] Implement user middleware replacement/insertion rules and protect required
  scaffolding from exclusion.
- [x] Implement system prompt composition without embedding provider-specific defaults.
- [x] Implement tool description overrides and late tool exclusion.
- [x] Set recursion and metadata defaults without exposing internal framework types.
- [x] Validate construction snapshots, tool schemas, prompt assembly, simple text
  response, one tool call, multiple tool calls, and error propagation.

Exit gate: a scripted model can construct and run a deep agent through the public Go
API with deterministic prompt, tool, state, and stream output.

### Phase 8 — Port filesystem middleware and permissions

- [x] Add `ls`, `read_file`, `write_file`, `edit_file`, `delete`, `glob`, `grep`, and
  conditional `execute` tools.
- [x] Match schema descriptions, pagination, line numbering, empty-file behavior,
  exact replacement counts, literal grep, output modes, match limits, and truncation
  notices.
- [x] Implement file/media type detection and safe binary representation without
  committing to optional video support.
- [x] Implement ordered allow/deny/ask permission rules, wildcard semantics, delete
  descendant checks, composite-route filtering, and approval generation.
- [x] Filter unsupported tools based on backend capabilities at each model call.
- [x] Implement large user/tool result eviction, filesystem offload, and state delta
  updates.
- [x] Port the Python backend and filesystem middleware tests, then add Go fuzz tests
  for paths, patterns, pagination, and permissions.

Exit gate: filesystem behavior and permission decisions match approved fixtures, and
shell authority cannot be gained through prompt or tool-name manipulation.

### Phase 9 — Port inline subagents

- [x] Define named inline subagent specifications, compiled runnable subagents, model,
  tools, middleware, skills, permissions, approval, and structured response options.
- [x] Implement subagent compilation through the generic agent factory.
- [x] Implement the task tool with agent selection, isolated messages, inherited
  context, selected state propagation, private-state exclusion, and result messages.
- [x] Add the default general-purpose subagent and exact override/disable behavior.
- [x] Preserve middleware/profile inheritance rules without sharing mutable state.
- [x] Propagate cancellation and bound concurrent subagent work.
- [x] Test nested delegation, unknown agent, state isolation, private fields,
  structured output, approval, failure, cancellation, and recursion limits.

Exit gate: inline and compiled subagents are behaviorally compatible and cannot leak
private parent or sibling state.

### Phase 10 — Port context management

- [x] Implement model-aware token thresholds and deterministic approximate fallback
  counting.
- [x] Implement automatic summarization triggers, cutoff selection, summary creation,
  preserved recent messages, and summary-message recognition.
- [x] Implement history offloading through backends with thread-aware paths and clear
  failure behavior.
- [x] Implement tool-argument truncation and large inline media offloading.
- [x] Implement the explicit compact tool and its state command updates.
- [x] Preserve tool-call/result validity when messages are summarized, removed,
  truncated, or replayed.
- [x] Add boundary tests around thresholds, malformed tool sequences, failed uploads,
  repeated compaction, checkpoint replay, and provider cursor reset.

Exit gate: long conversations compact without invalid message sequences, data loss,
or checkpoint divergence.

### Phase 11 — Port skills and memory

- [x] Define skill source, metadata, validation limits, compatibility fields, allowed
  tools, and error reporting.
- [x] Implement skill discovery and loading through any conforming backend.
- [x] Implement stable ordering, duplicate handling, malformed metadata warnings, and
  bounded file size/error output.
- [x] Inject skill locations and summaries at the same middleware boundary as Python.
- [x] Load memory files before agent execution, strip only intended comments, and
  inject memory at model-call time.
- [x] Preserve prompt cache placement rules through a provider capability rather than
  hard-coded model classes.
- [x] Test state reuse, async cancellation equivalents, missing paths, partial errors,
  duplicates, large files, prompt ordering, and cache metadata.

Exit gate: skills and memory work across state, filesystem, store, and composite
backends with stable prompts and no hidden provider dependency.

### Phase 12 — Complete persistence, streaming, and approval behavior

- [x] Integrate the Phase 4 SQLite and PostgreSQL savers with full interrupted agent
  runs after the in-memory runtime contract is stable; keep their database drivers in
  optional packages.
- [x] Add a durable namespaced store adapter and versioned migrations.
- [x] Finalize checkpoint payload and stream schema versioning without widening the
  Phase 4 safe interoperability subset implicitly.
- [x] Complete token, message, update, task, lifecycle, interrupt, and custom event
  streams.
- [x] Guarantee backpressure, cancellation, error delivery, and cleanup across model,
  tool, subagent, and checkpoint streams.
- [x] Complete approval edit/reject behavior, multiple pending calls, resume payload
  validation, and persistence across process restart.
- [x] Add crash/restart, partial write, replay, duplicate resume, cancellation, race,
  and migration tests.

Exit gate: durable interrupted runs can restart and resume exactly once, and stream
consumers cannot leak goroutines or silently lose terminal errors.

### Phase 13 — Profiles, structured grading, and provider adapters

- [x] Port profile keys, registration, merge semantics, built-in selection, prompt
  assembly, tool description overrides, required middleware protection, and exclusion
  coverage checks.
- [x] Keep provider and harness profiles separate and composable.
- [x] Port provider-neutral prompt-caching middleware and add adapter-specific cache
  metadata translation.
- [x] Port rubric evaluation and structured grading with explicit fallback behavior.
- [x] Add focused provider adapters in separate packages, beginning with providers
  selected during Phase 1.
- [x] Run shared provider conformance tests for text, tool calls, parallel calls,
  streaming, usage, structured output, context overflow, cancellation, and provider
  errors.
- [x] Keep provider-specific compatibility shims isolated and optional.

Exit gate: core behavior is provider-neutral, selected adapters pass the same model
contract, and profile behavior is reproducible from fixtures.

### Phase 14 — Evaluate deferred integrations and release readiness

- [x] Implement provider-neutral asynchronous subagent lifecycle tools and durable
  delta task state without a hosted API dependency.
- [ ] Add the remote Agent Protocol client as an optional adapter.
- [x] Defer video decoding and heavy media dependencies. Video/file blocks and
  backend offload remain supported as opaque media records.
- [x] Keep hosted sandbox, tracing, context-hub, and deployment integrations as
  separate adapters; include the LangSmith sandbox and persistent Context Hub
  connectors without making either a core runtime dependency.
- [x] Keep ACP and a general CLI downstream. Include Shelley as an end-to-end example
  application with its own documented single-user security boundary.
- [x] Port only the evaluation scenarios needed to measure agent quality and parity;
  keep networked evaluations separate from deterministic tests.
- [x] Complete public API review, examples, security guidance, migration notes,
  compatibility matrix, notices, and release checklist.

Exit gate: deferred items have explicit decisions, the public SDK is documented and
stable, and release gates do not depend on networked evaluations.

## Verification strategy

### Test layers

1. Unit tests for reducers, messages, schemas, middleware composition, routing,
   permissions, truncation, and serialization.
2. Contract tests shared by every model, tool, backend, store, cache, and checkpoint
   implementation.
3. Graph integration tests for concurrency, retries, streaming, checkpoints,
   interrupts, replay, and cancellation.
4. Deep Agent black-box tests driven by scripted model transcripts.
5. Cross-language fixture tests generated from approved Python and TypeScript cases,
   including schema-identical SQLite/PostgreSQL databases and the documented safe
   checkpoint payload subset.
6. Provider integration tests isolated behind credentials and explicit test flags.
7. Race, fuzz, leak, and fault-injection tests for the runtime and backends.
8. Evaluation suites kept separate from correctness tests.

### Required local gates

- `gofmt` check for all Go source.
- `go vet ./...`.
- `go test ./...`.
- `go test -race ./...` for supported packages.
- Fuzz seeds for state reducers, message reduction, path normalization, permission
  matching, checkpoint decoding, and stream cancellation.
- A generated-fixture drift check.
- A compatibility-matrix completeness check.

### Parity fixture rules

- Every fixture records upstream repository, revision, source test/symbol, and whether
  behavior is normative or intentionally different.
- Generated fixtures are updated through their source and generator only.
- Snapshot comparisons normalize only documented nondeterminism such as IDs,
  timestamps, and provider request identifiers.
- Prompt and tool-schema snapshots remain byte-sensitive after normalization because
  small differences affect model behavior.
- A source bug may be documented in an exploratory reproduction, but the passing Go
  suite must assert the intended correct behavior.

## Upstream update process

For each upstream refresh:

- [x] Fast-forward the reference checkout and record the new revision.
- [x] Diff only the source files and tests listed in the upstream manifest first.
- [x] Re-run the import inventory to discover new framework dependencies.
- [x] Map changed upstream tests to compatibility rows.
- [x] Regenerate fixtures from source inputs.
- [x] Review every changed prompt, tool schema, middleware hook, state reducer,
  checkpoint field, and stream event manually.
- [x] Update intentional differences and migration notes.
- [x] Run all deterministic, race, fuzz-seed, and provider contract gates.

Do not update compatibility claims merely because the Go suite remains green; a green
suite with stale fixtures is not evidence of current parity.

## Major risks and controls

| Risk | Control |
|---|---|
| Accidentally porting entire frameworks | Enforce the import inventory and required dependency cut; every new framework symbol needs a Deep Agents use case |
| Python dynamic state does not map cleanly to Go | Use a reducer-backed state bag with typed built-in helpers and explicit validation |
| Middleware order subtly changes behavior | Maintain nesting/order fixtures and construction snapshots |
| Concurrent tools make state nondeterministic | Define deterministic send/result ordering and reducer conflict rules before parallel execution |
| Streaming leaks goroutines or loses errors | Specify ownership, backpressure, cancellation, close, and terminal error behavior; run race/leak tests |
| Checkpoints cannot evolve safely | Preserve pinned migration history, version language-neutral records, and test older database fixtures |
| Schema compatibility is mistaken for payload compatibility | Track and test them separately; publish the safe payload support table and reject unsupported tags explicitly |
| Python deserialization behavior creates a code-execution boundary | Never port pickle, imports, arbitrary constructors, or serialized callables; use an allowlisted plain-data codec |
| Filesystem tools escape their authority | Normalize paths, verify roots and symlinks, enforce permissions in backend/tool code, and default shell off |
| Provider differences contaminate core APIs | Use capability interfaces and isolated adapters with shared contract tests |
| Prompt or schema drift degrades quality | Keep byte-sensitive versioned snapshots tied to upstream revisions |
| Stale or wrong Python source produces false parity | Keep the separate pinned Python checkout in the manifest and fail drift checks when its revision changes |
| Existing Go experiment biases the design | Treat it as non-normative; accept only behavior backed by current upstream contracts |
| Private upstream APIs change without notice | Replace private dependencies with local stable contracts and track their semantics in fixtures |

## Resolved implementation decisions

- [x] Public module path `github.com/semistrict/dago` and repository initialization
  on `main`.
- [x] Go 1.26 minimum with Linux and macOS CI.
- [x] Behavioral parity targets the exact pinned Python revisions in the upstream
  manifest.
- [x] Checkpoint compatibility boundary: match the standard SQLite/PostgreSQL schemas
  and saver semantics; exchange only the documented safe language-neutral payload
  subset; do not port Python-specific encodings.
- [x] Delta channels are required across the runtime, in-memory saver, SQLite saver,
  and PostgreSQL saver; there is no non-delta compatibility tier.
- [x] Keep the minimal graph runtime internal until a separate public-runtime proposal
  is reviewed.
- [x] Keep the core model interfaces independent and put focused adapters in separate
  packages.
- [x] Ship a deterministic scripted model and one focused OpenAI adapter first.
- [x] Ship standard SQLite and PostgreSQL savers; defer the PostgreSQL shallow saver
  and graph vector stores.
- [x] Verify the core agent, filesystem, permissions, inline subagents, summarization,
  skills, memory, persistence, streaming, and approval tier; defer hosted graph and
  video-decoding integrations.

## Definition of done for the overall port

- [x] Every non-deferred compatibility row is `verified` against an anchored source
  revision.
- [x] Every intentional difference is documented with rationale and user impact.
- [x] Public examples compile and run against scripted models without network access.
- [x] All concrete backends, model adapters, stores, caches, and checkpoint savers pass
  their shared conformance suites.
- [x] Deterministic tests, race tests, fuzz seeds, leak checks, and fixture drift checks
  pass.
- [x] Filesystem and shell security boundaries have dedicated adversarial tests and
  documentation.
- [x] Durable checkpoints survive restart, migration, interrupt, and resume tests.
- [x] Delta channels pass runtime, history, SQLite, PostgreSQL, cross-language,
  migration, and snapshot-aware pruning tests without a non-delta fallback path.
- [x] SQLite and PostgreSQL schemas, migrations, saver operations, and safe payload
  fixtures pass bidirectional checks against the pinned Python implementation.
- [x] Python-specific checkpoint payloads fail with documented typed errors and never
  trigger imports, constructors, callable execution, or partial state loss.
- [x] Streaming cancellation and terminal error behavior are verified at every nested
  execution layer.
- [x] Licenses, notices, provenance, supported versions, compatibility claims, and
  deferred scope are documented.
- [x] No implementation package claims full LangChain or LangGraph compatibility
  beyond the tested subset required by Deep Agents.
