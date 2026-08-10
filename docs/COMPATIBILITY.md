# Compatibility matrix

Status values are `verified`, `implemented`, `deferred`, and `intentionally different`.

| Surface | Status | Evidence or boundary |
|---|---|---|
| Explicit model and custom tool construction | verified | `agent/agent_test.go`, `dago_test.go` |
| Middleware order and wrapper nesting | verified | `agent/agent_test.go` |
| Parallel tool calls and deterministic reduction | verified | agent and graph tests |
| Provider and synthetic-tool structured output | verified | schema validation and retry tests |
| Mandatory delta state and message channels | verified | channel, graph, memory saver, SQLite, remove-all reset, and pre-checkpoint stable-message-ID tests |
| Graph routing, sends, retry, interrupt, resume, and parent handoff preservation | verified | graph tests plus agent after-hook model re-entry, invalid-destination guards, and typed terminal handoff tests |
| Replay by checkpoint and thread fork | verified | public history/replay/fork helpers and saver copy/replay tests |
| SQLite standard schema and saver behavior | verified | schema, conflict, history, copy, prune, restart tests |
| PostgreSQL migrations 0–9 and saver behavior | verified | migration tests and live saver integration gate |
| Safe checkpoint payload subset | verified | serde round trips and rejection tests |
| Bidirectional Python payload fixtures | verified | SQLite and PostgreSQL are generated, read, and continued in both directions |
| State, memory, store, filesystem, composite, local shell | verified | shared backend and thread-scoped delta-state tests |
| LangSmith remote sandbox | verified | SDK adapter tests; live test is credential-gated |
| Context Hub persistent agent repository | verified | lazy pull, commit chaining, linked entries, cache recovery, batch transfer, and LangSmith SDK transport tests |
| Filesystem tools and permission approval | verified | root vertical-slice tests |
| Declarative and precompiled subagents, isolation, state propagation, and nested approval resume | verified | root and agent tests |
| Summarization, offload, and compaction | verified | AND/OR/fraction threshold clauses, valid cutoffs, thread-aware offload, and state-update tests |
| Skills and memory prompt injection | verified | safe YAML, deterministic discovery, warning, ordering, and prompt tests |
| Provider and harness profiles | verified | built-in Anthropic and Nemotron harness overlays plus OpenAI/NVIDIA/OpenRouter construction defaults; active repair, retry, budget, policy, entity, follow-up, and final-answer contracts; additive registration and override tests |
| Token/update/task/interrupt/custom streaming | verified | graph, agent, and provider stream tests |
| API-key and subscription OAuth model access | verified | request, PKCE, refresh, persistence, and stream tests |
| Tracing/evaluation integration | deferred | optional; not needed by the local execution contract |
| Asynchronous hosted-subagent lifecycle and durable task state | verified | provider-neutral runner and five management-tool tests |
| Remote Agent Protocol background client | verified | thread/run create, status/result, interrupting update, cancellation, auth, path escaping, and redirect-boundary tests |
| Video processing | verified | pluggable extractor contract plus optional bounded FFmpeg adapter; video-window, frame, truncation, fallback, and failure tests |
| Shelley end-to-end application | verified | HTTP route tests plus desktop/mobile browser interaction checks |

## Intentional differences

- Go exposes one context-aware API rather than Python sync and async variants.
- Python decorators, runtime imports, reflection-driven runnable composition, and
  class hierarchy are replaced by small Go interfaces.
- Pickle, arbitrary constructor tags, Pydantic and NumPy reconstruction, and
  serialized callables are rejected. This is a security boundary, not missing parity.
- Shell execution is absent unless an explicit sandbox or local-shell backend is
  constructed. The local shell is documented as trusted-host execution, not isolation.
- Provider credentials are explicit adapter inputs or a library-owned OAuth session;
  credentials from another application are never discovered or copied.
- Python's in-process ASGI transport for remote subagents is not applicable in Go;
  async subagents require an HTTP URL or a caller-supplied runner.
- Python package-version inspection is not applied to Go provider adapters. Canonical
  Dago messages already preserve tool-call and tool-result identity, so the Nemotron
  pre-serialization compatibility layer is an explicit no-op at the core boundary.
- A tool command targeting its parent is returned as a typed terminal handoff for the
  enclosing Go orchestrator to route. Dago does not expose a general-purpose public
  graph builder merely to reproduce Python's graph-composition syntax.
- Video decoding is an opt-in extractor capability. The supplied FFmpeg adapter is
  subprocess-isolated by context and output limits; opaque media behavior remains
  the default and no decoder library is a core dependency.
- General tracing integrations remain separate optional adapters and are not core
  runtime dependencies.
