# QuickJS upstream test port

The normative source is `deepagents-quickjs` at revision
`217b9eb372fa51b0439434f31abc3ac22e6cd7f2` in
[`upstream-manifest.json`](upstream-manifest.json). Its 190 test functions are
tracked below by source suite. The Go port consolidates parametrized and
sync/async duplicates around observable contracts rather than preserving the
Python test layout.

## Coverage map

| Upstream suite | Functions | Local evidence | Disposition |
|---|---:|---|---|
| `tests/unit_tests/test_end_to_end.py` | 16 | `interpreter_agent_upstream_test.go`, `interpreter_upstream_test.go` | Agent tool loop, native PTC values, runtime injection, failures, budgets, invalid arguments, and parallel threads are ported. |
| `tests/unit_tests/test_end_to_end_async.py` | 8 | same as sync plus `internal/quickjs/engine_test.go` | Go has one context-aware path; async host calls, cancellation, and concurrent promises are exercised through that path. |
| `tests/unit_tests/test_prompt_modes.py` | 3 | `interpreter_upstream_test.go` | Durable-thread prompt and typed PTC signatures are ported. Turn/call modes are outside the Go API. |
| `tests/unit_tests/test_ptc.py` | 45 | `interpreter_upstream_test.go`, `datool/tool_test.go` | Allowlisting, self-exclusion, native JSON, omitted `undefined`, concurrency, last text content, namespace replacement, safe names, budgets, failures, runtime forwarding, and prompt input types are ported. |
| `tests/unit_tests/test_repl_middleware.py` | 70 | `interpreter_test.go`, `interpreter_upstream_test.go`, `internal/quickjs/engine_test.go` | Registration, persistence, isolation, syntax/runtime/timeout/deadlock errors, bounded console output, opaque values, Promise unwrapping, truncation, task dispatch, and concurrent-eval rejection are ported. Python worker-registry lifecycle tests do not apply. |
| `tests/unit_tests/test_snapshot.py` | 23 | `interpreter_snapshot_upstream_test.go`, `internal/quickjs/engine_test.go`, `internal/wafl/*_test.go` | Snapshot validation, round trips, chain associativity, clear/re-anchor, unknown records, clone isolation, caps, resume, and compact deltas are ported using WAFL dirty pages. |
| `tests/unit_tests/test_snapshot_persistence.py` | 4 | `interpreter_snapshot_upstream_test.go`, `interpreter_agent_upstream_test.go` | `const`, `let`, and top-level-await bindings persist through evals, turns, and saver reloads. Turn-reset mode is outside the Go API. |
| `tests/unit_tests/test_subagent_events.py` | 9 | `dago_test.go`, `interpreter_agent_upstream_test.go` | Started/completed/error child lifecycle and nested updates are covered by the shared subagent event contract; interpreter dispatch uses `tools.task`. |
| `tests/unit_tests/test_thread_affinity.py` | 3 | `interpreter_upstream_test.go`, `interpreter_agent_upstream_test.go` | Runtime/config propagation and subagent dispatch are ported. Python event-loop identity has no Go equivalent. |
| `tests/unit_tests/smoke_tests/test_system_prompt.py` | 2 | `interpreter_upstream_test.go`, `interpreter_agent_upstream_test.go` | Required durable-REPL and PTC guidance is asserted semantically rather than as a Python prompt snapshot. |
| `tests/integration_tests/test_postgres.py` | 1 | `interpreter_agent_upstream_test.go` | PostgreSQL checkpoint persistence is gated by `DAGO_POSTGRES_TEST_DSN`; event-loop identity does not apply. |
| `tests/integration_tests/test_rlm.py` | 2 | `interpreter_agent_upstream_test.go` | PTC subagent dispatch and allowlisting are ported through the native `task` tool. |
| `tests/benchmarks/test_quickjs_memory.py` | 2 | `internal/quickjs/engine_test.go` | Concurrent-engine and snapshot-turn allocation workloads are Go benchmarks. |
| `tests/benchmarks/test_quickjs_throughput.py` | 2 | `internal/quickjs/engine_test.go` | PTC-plus-console and snapshot-restore turn throughput are Go benchmarks. |

## Intentional API boundaries

- The interpreter is durable per checkpointed thread. Python-only `turn` and
  `call` lifecycle modes are not exposed.
- PTC configuration is a Go `[]string` allowlist resolved against agent tools.
  Python unions accepting tool objects and runtime-invalid booleans or mappings
  do not exist in the typed API.
- Tool names are converted to safe JavaScript identifiers and host bindings use
  hashed internal names. Upstream's skip/reject behavior for invalid JavaScript
  identifiers is therefore replaced by a safe deterministic mapping.
- The shared Go tool definition currently describes input JSON only, so prompt
  assertions cover input signatures and use `Promise<unknown>` for returns.
  Native structured return values are nevertheless preserved by the PTC bridge.
- Python worker threads, destructor behavior, traceback-cycle cleanup, Pydantic
  schema-title repair, and event-loop affinity are implementation details with
  no Go runtime counterpart.
- Snapshot patch records are WAFL-instrumented 4 KiB dirty pages applied to the
  pinned quickjs-rs whole-memory envelope, not Python `bsdiff` payloads.
- WAFL tracking is enabled for every interpreter instance and has no production
  opt-out. The benchmark's synthetic untracked case exists only to measure the
  tracking overhead against a controlled baseline.

Run the deterministic port with `go test ./internal/quickjs ./internal/wafl .`.
Run end-to-end interpreter, memory-tracking, snapshot, large-tool-output, and
instrumentation benchmarks with:

```sh
go test . ./internal/quickjs ./internal/wafl -run '^$' -bench . -benchmem
```
