# dago-native shelley-in-dago

The original Shelley's pinned upstream tests are the behavioral specification. Production code
may preserve package names, exported identifiers, and concrete facade types required
by those tests, but the executable runtime must use dago's model, message, tool,
agent, backend, interrupt, stream, and checkpoint contracts directly.

## Invariants

- Every upstream test artifact and named Go case from revision
  `1d4cbe79c6be45cc0105d46819cb54844f98eddd` remains represented. UI test-call
  counts may grow but may not shrink.
- Tests migrate to dago-native contracts with the production surface they exercise;
  they are not frozen around removed shelley-in-dago model, message, or tool types.
- Temporary compatibility facades are deleted once their corresponding upstream
  tests have migrated. No executable compatibility path is an acceptable endpoint.
- dago checkpoints are the canonical execution state. The shelley-in-dago database is a UI and
  product read model populated from dago events, not a second agent state machine.
- The running application invokes a dago `damodel.Chat` directly and executes dago
  `datool.Tool` implementations directly. It must not convert a native dago model or
  tool into a shelley-in-dago request and then convert it back into dago.
- Approvals and other pauses are dago interrupts resumed through `dagent.Input.Resume`.
- Cancellation is resolved through dago's durable cancellation operation.

## Target dependency direction

```text
shelley-in-dago HTTP/UI server
  -> dago deep agent
     -> dago damodel.Chat
     -> dago datool.Tool
     -> dago dabackend.Backend
     -> dago dacheckpoint.Saver
  -> shelley-in-dago database projection
  -> shelley-in-dago SSE/UI event projection
```

The former `dagoruntime` migration package and the shelley-in-dago model-service facade have
been removed from the executable dependency graph. Provider construction returns a
dago `damodel.Chat` directly, tools implement `datool.Tool` directly, and the remaining
runtime projection lives inside `loop`. Test-only probes may translate native results
into assertion-friendly values, but they do not introduce an executable fallback.

## Replacement matrix

| Upstream surface | Native owner | Required result |
|---|---|---|
| `loop` | `dagent.Agent` | `Loop` becomes a thin session facade; no model/tool state machine remains |
| `llm.Message`, `Content`, `Request`, `Response` | `damessage.Message`, `damodel.Request`, `damodel.Response` | Upstream shapes become boundary facades and database codecs only |
| `llm/ant`, `llm/gem`, `llm/oai` | dago OpenAI Responses provider | Every preserved provider case is explicitly assigned to its OpenAI Responses equivalent; removed vendor protocols have no executable facade |
| `claudetool` | `datool.Tool` plus dago backends | Tool implementations receive dago runtime/state directly; no `llm.Tool` execution path in the binary |
| `models`, `modelsources` | dago model registry/factories | Catalog APIs remain, but every ready runtime model exposes a native `damodel.Chat` |
| conversation approvals | dago interrupts | UI approval endpoints persist and resume native interrupt values |
| conversation cancellation | dago cancellation | No separate pending-tool or pending-model scheduler in shelley-in-dago |
| message and tool streaming | dago stream events | shelley-in-dago translates events to its established SSE payloads without reconstructing execution order |
| conversation history | dago checkpoint state | shelley-in-dago message rows are an append-only projection used by the UI, search, export, and audit features |
| subagents, skills, memory, summary, todo | dago middleware | shelley-in-dago supplies configuration and UI projection rather than parallel implementations |

## Test ownership

The pinned corpus contains 298 artifacts, including 169 Go test files and 63 UI
unit/end-to-end TypeScript files. The main implementation groups are:

- Model and provider contracts: `llm`, `llm/ant`, `llm/gem`, `llm/oai`,
  `llm/llmhttp`, `models`, and `modelsources`.
- Agent and tools: `loop`, `claudetool`, `skills`, and subagent server tests.
- Persistence and orchestration: `db` and `server`.
- Product infrastructure: `gitstate`, `dtach`, `featureflags`, `slug`,
  `subpub`, CLI, client, and templates.
- Browser behavior: UI unit tests, Playwright specifications, and LazyCue fixtures.

Product infrastructure is not replaced merely because dago owns agent execution.
It is rebuilt only where its implementation depends on a legacy model/tool state
machine.

Provider retryability, attempt metadata, finish reasons, refusal details, usage,
reasoning state, citations, and provider-hosted tool blocks are native dago model
metadata. shelley-in-dago translates them only when writing its established database and UI
records. Callers that own a higher-level retry budget disable the provider retry loop
explicitly, so retry layers cannot multiply transport attempts.

## Completion evidence

Completion requires all of the following:

1. `make drift` proves generated compatibility fixtures match their checked-in inputs
   and validates any explicitly configured reference checkout.
2. Every current Go package test passes, including race runs and Linux-only cases.
3. Every current UI unit test, Playwright specification, and LazyCue scenario passes.
4. Provider integration suites pass against the native dago provider implementations.
5. Browser tests prove approvals, continuation, cancellation, tool ordering, streaming,
   and configured-provider behavior through the real application.
6. The executable dependency graph contains no `dagoruntime` package and no reachable
   legacy shelley-in-dago model/tool loop.
