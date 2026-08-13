# Compatibility matrix

Status values are `verified`, `implemented`, `deferred`, and `intentionally different`.

| Surface | Status | Evidence or boundary |
|---|---|---|
| Explicit model and custom tool construction | verified | `dagent/agent_test.go`, `dago_test.go` |
| Typed state fields, reads, dependencies, metadata, resume, and structured output | verified | typed adapter, checkpoint-shaped decode, approval-resume, metadata, and schema-validation tests |
| Middleware order and wrapper nesting | verified | `dagent/agent_test.go` |
| Parallel tool calls and deterministic reduction | verified | agent and graph tests |
| Provider and synthetic-tool structured output | verified | schema validation and retry tests |
| Mandatory delta state and message channels | verified | channel, graph, memory saver, SQLite, remove-all reset, and pre-checkpoint stable-message-ID tests |
| Graph routing, sends, retry, interrupt, resume, and parent handoff preservation | verified | graph tests plus agent after-hook model re-entry, invalid-destination guards, and typed terminal handoff tests |
| Replay by checkpoint and thread fork | verified | public history/replay/fork helpers and saver copy/replay tests |
| SQLite standard schema and saver behavior | verified | schema, conflict, history, copy, prune, restart tests |
| PostgreSQL migrations 0–9 and saver behavior | verified | migration tests and live saver integration gate |
| Safe checkpoint payload subset | verified | serde round trips, portable named-scalar normalization, approval-interrupt persistence, and rejection tests |
| Bidirectional Python payload fixtures | verified | SQLite and PostgreSQL are generated, read, and continued in both directions |
| State, memory, store, filesystem, composite, local shell | verified | shared backend and thread-scoped delta-state tests |
| Persistent JavaScript interpreter and programmatic tool calling | verified | normal Go builds use the pinned quickjs-rs WASM guest under Wazero's portable interpreter; TinyGo builds reject explicit enablement and Shelley omits `js_eval`; full applicable upstream suite map in `docs/QUICKJS_TEST_PORT.md`; Promise settlement, typed PTC, interruption, WAFL dirty-page checkpoints, memory/SQLite/PostgreSQL restore, subagent dispatch, and browser execution tests |
| Rooted local filesystem confinement | verified | `os.Root` operations plus traversal, symlink-escape, write, delete, glob, grep, upload, and download tests |
| Filesystem result safety, media compaction, partial/uncapped grep, and transfer batching | verified | root filesystem contracts plus backend concurrency, pagination, deterministic grep, partial-error, and composite batch tests |
| LangSmith remote sandbox | verified | SDK adapter tests; live test is credential-gated |
| Docker local sandbox | implemented | hardened creation, workspace confinement, execution, cancellation restart, cleanup, and opt-in live-container tests |
| Context Hub persistent agent repository | verified | lazy pull, commit chaining, linked entries, cache recovery, batch transfer, and LangSmith SDK transport tests |
| Filesystem tools and permission approval | verified | root vertical-slice tests |
| Declarative and precompiled subagents, isolation, state propagation, and nested approval resume | verified | root and agent tests |
| Inline subagent todo isolation and operational failure propagation | verified | root subagent isolation, recoverable-argument, and child failure tests |
| Invocation-scoped runtime context | verified | graph runtime, concurrent invocation, inline subagent, and rubric grader propagation tests |
| Durable rubric terminal outcomes | verified | public result and persisted-checkpoint status tests for terminal grading outcomes |
| Task-scoped structured output for declarative subagents | verified | per-task schema validation and precompiled-runnable rejection tests |
| Summarization, offload, and compaction | verified | AND/OR/fraction threshold clauses, valid cutoffs, thread-aware offload, and state-update tests |
| Skills and memory prompt injection | verified | safe YAML, deterministic discovery, warning, ordering, and prompt tests |
| Workspace instruction discovery | verified | `daworkspace` precedence, trust gating, deduplication, scoped-file, directory-filter, and JSON-contract tests plus shelley-in-dago prompt coverage |
| Provider and harness profiles | verified | built-in Anthropic harness overlays; explicit Nemotron profile composition; explicit OpenAI/NVIDIA/OpenRouter construction-profile sets; active repair, retry, budget, policy, entity, follow-up, and final-answer contracts |
| Token/update/task/interrupt/custom streaming | verified | graph, agent, and provider stream tests |
| Agent-event and model-chunk iterators | verified | completion, terminal-error, and early-break closure tests while retaining explicit `Next`/`Close` |
| API-key and subscription OAuth model access | verified | request, PKCE, refresh, persistence, HTTP streaming, default Responses WebSocket transport, incremental continuation, remote V2 compaction trigger/state replay, and cancellation tests |
| OpenRouter Responses model access | verified | API-key authentication, app attribution, provider routing, tool requests, usage, streaming keepalives, typed errors, and an opt-in live structured/tool/stream suite |
| Tracing/evaluation integration | deferred | optional; not needed by the local execution contract |
| Asynchronous hosted-subagent lifecycle and durable task state | verified | provider-neutral runner and five management-tool tests |
| Remote Agent Protocol background client | verified | thread/run create, status/result, interrupting update, cancellation, auth, path escaping, and redirect-boundary tests |
| LangSmith Studio / Agent Server development API | verified | info and schema discovery; assistant, thread, run, checkpoint state/history/update/fork, store, cancellation, replayable SSE, CORS, generated-wrapper, and config tests |
| Video processing | verified | pluggable extractor contract plus optional bounded FFmpeg adapter; video-window, frame, truncation, fallback, and failure tests |
| Executable upstream conformance provenance | verified | generator validates pinned source paths and test selectors; generated-contract tests strictly decode, validate, mutate, and round-trip every fixture |
| shelley-in-dago end-to-end application | verified | HTTP route tests plus desktop/mobile browser interaction checks |

## Intentional differences

- Go exposes one context-aware API rather than Python sync and async variants.
- The JavaScript interpreter exposes durable checkpointed-thread state rather
  than Python's additional turn-reset and per-call lifecycle modes. Its PTC
  configuration is a typed tool-name allowlist; JavaScript names are safely
  normalized instead of being rejected solely for identifier syntax.
- Python decorators, runtime imports, reflection-driven runnable composition, and
  class hierarchy are replaced by small Go interfaces.
- Pickle, arbitrary constructor tags, Pydantic and NumPy reconstruction, and
  serialized callables are rejected. This is a security boundary, not missing parity.
- Shell execution is absent unless an explicit sandbox or local-shell backend is
  constructed. The local shell is documented as trusted-host execution, not isolation.
- Provider credentials are explicit adapter inputs or a library-owned OAuth session;
  credentials from another application are never discovered or copied.
- Python's in-process ASGI transport for remote subagents is not applicable in Go;
  async subagents require a caller-supplied runner. The optional Agent Protocol
  package constructs an HTTP-backed runner from URL and authentication options.
- Python package-version inspection is not applied to Go provider adapters. Canonical
  dago messages already preserve tool-call and tool-result identity, so the Nemotron
  pre-serialization compatibility layer is an explicit no-op at the core boundary.
- A tool command targeting its parent is returned as a typed terminal handoff for the
  enclosing Go orchestrator to route. dago does not expose a general-purpose public
  graph builder merely to reproduce Python's graph-composition syntax.
- Video decoding is an opt-in extractor capability. The supplied FFmpeg adapter is
  subprocess-isolated by context and output limits; opaque media behavior remains
  the default and no decoder library is a core dependency.
- General tracing integrations remain separate optional adapters and are not core
  runtime dependencies.
- The local Agent Server implements the Studio and SDK surface needed for dago
  development. It does not execute arbitrary Python/JavaScript graph imports, host
  custom web applications, schedule crons, provide LangSmith tracing, or implement
  the protocol-v2 long-lived thread websocket. Go graph paths are compiled into a
  generated wrapper, and runs use the existing dago graph rather than a general
  LangGraph runtime.
