# Compatibility matrix

Status values are `verified`, `implemented`, `deferred`, and `intentionally different`.

| Surface | Status | Evidence or boundary |
|---|---|---|
| Explicit model and custom tool construction | verified | `agent/agent_test.go`, `dago_test.go` |
| Middleware order and wrapper nesting | verified | `agent/agent_test.go` |
| Parallel tool calls and deterministic reduction | verified | agent and graph tests |
| Provider and synthetic-tool structured output | verified | schema validation and retry tests |
| Mandatory delta state and message channels | verified | channel, graph, memory saver, SQLite tests |
| Graph routing, sends, retry, interrupt, resume | verified | `internal/graph/graph_test.go` |
| Replay by checkpoint and thread fork | verified | public history/replay/fork helpers and saver copy/replay tests |
| SQLite standard schema and saver behavior | verified | schema, conflict, history, copy, prune, restart tests |
| PostgreSQL migrations 0–9 and saver behavior | verified | migration tests and live saver integration gate |
| Safe checkpoint payload subset | verified | serde round trips and rejection tests |
| Bidirectional Python payload fixtures | verified | SQLite and PostgreSQL are generated, read, and continued in both directions |
| State, memory, store, filesystem, composite, local shell | verified | shared backend and thread-scoped delta-state tests |
| LangSmith remote sandbox | verified | SDK adapter tests; live test is credential-gated |
| Filesystem tools and permission approval | verified | root vertical-slice tests |
| Declarative and precompiled subagents, isolation, state propagation, and nested approval resume | verified | root and agent tests |
| Summarization, offload, and compaction | verified | threshold, valid-cutoff, thread-aware offload, and state-update tests |
| Skills and memory prompt injection | verified | safe YAML, deterministic discovery, warning, ordering, and prompt tests |
| Provider and harness profiles | verified | additive registration, provider/exact resolution, hook/factory chaining, option precedence, prompt composition, exclusions, and worker overrides |
| Token/update/task/interrupt/custom streaming | verified | graph, agent, and provider stream tests |
| API-key and subscription OAuth model access | verified | request, PKCE, refresh, persistence, and stream tests |
| Tracing/evaluation integration | deferred | optional; not needed by the local execution contract |
| Asynchronous hosted-subagent lifecycle and durable task state | verified | provider-neutral runner and five management-tool tests |
| Remote Agent Protocol client | deferred | optional adapter; core runner contract has no hosted dependency |
| Video processing | deferred | media blocks round-trip; no heavy decoder dependency |
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
- Parent-graph commands and a public general-purpose graph builder are deferred. The
  internal command/send surface implements only the routing used by the agent factory.
- Remote Agent Protocol clients, video decoding, and tracing integrations remain
  separate optional adapters and are not core runtime dependencies.
