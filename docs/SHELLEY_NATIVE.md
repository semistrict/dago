# Dago-native Shelley

Shelley's pinned upstream tests are the behavioral specification. Production code
may preserve package names, exported identifiers, and concrete facade types required
by those tests, but the executable runtime must use Dago's model, message, tool,
agent, backend, interrupt, stream, and checkpoint contracts directly.

## Invariants

- Upstream test artifacts remain byte-for-byte identical to revision
  `1d4cbe79c6be45cc0105d46819cb54844f98eddd`.
- Tests are never changed to accept a Dago-specific result or implementation type.
- A compatibility facade is acceptable only when an upstream test compiles against
  it and the production executable does not use the legacy implementation behind it.
- Dago checkpoints are the canonical execution state. Shelley's database is a UI and
  product read model populated from Dago events, not a second agent state machine.
- The running application invokes a Dago `model.Chat` directly and executes Dago
  `tool.Tool` implementations directly. It must not convert a native Dago model or
  tool into a Shelley request and then convert it back into Dago.
- Approvals and other pauses are Dago interrupts resumed through `agent.Input.Resume`.
- Cancellation is resolved through Dago's durable cancellation operation.

## Target dependency direction

```text
Shelley HTTP/UI server
  -> Dago deep agent
     -> Dago model.Chat
     -> Dago tool.Tool
     -> Dago backend.Backend
     -> Dago checkpoint.Saver
  -> Shelley database projection
  -> Shelley SSE/UI event projection
```

The `dagoruntime` package is temporary migration code. Completion removes it from
the executable dependency graph. Test-only facade helpers may remain elsewhere when
the pinned suite directly names an upstream type.

## Replacement matrix

| Upstream surface | Native owner | Required result |
|---|---|---|
| `loop` | `dago.DeepAgent` and `agent.Agent` | `Loop` becomes a thin session facade; no model/tool state machine remains |
| `llm.Message`, `Content`, `Request`, `Response` | `message.Message`, `model.Request`, `model.Response` | Upstream shapes become boundary facades and database codecs only |
| `llm/ant`, `llm/gem`, `llm/oai` | Dago provider packages | Provider behavior passes the original provider suites through Shelley facade types |
| `claudetool` | `tool.Tool` plus Dago backends | Tool implementations receive Dago runtime/state directly; no `llm.Tool` execution path in the binary |
| `models`, `modelsources` | Dago model registry/factories | Catalog APIs remain, but every ready runtime model exposes a native `model.Chat` |
| conversation approvals | Dago interrupts | UI approval endpoints persist and resume native interrupt values |
| conversation cancellation | Dago cancellation | No separate pending-tool or pending-model scheduler in Shelley |
| message and tool streaming | Dago stream events | Shelley translates events to its established SSE payloads without reconstructing execution order |
| conversation history | Dago checkpoint state | Shelley message rows are an append-only projection used by the UI, search, export, and audit features |
| subagents, skills, memory, summary, todo | Dago middleware | Shelley supplies configuration and UI projection rather than parallel implementations |

## Test ownership

The pinned corpus contains 298 artifacts, including 169 Go test files and 63 UI
unit/end-to-end TypeScript files. The main implementation groups are:

- Model and provider contracts: `llm`, `llm/ant`, `llm/gem`, `llm/oai`,
  `llm/llmhttp`, `models`, and `modelsources`.
- Agent and tools: `loop`, `claudetool`, `skills`, and subagent server tests.
- Persistence and orchestration: `db` and `server`.
- Product infrastructure: `gitstate`, `dtach`, `exeenv`, `featureflags`, `slug`,
  `subpub`, CLI, client, and templates.
- Browser behavior: UI unit tests, Playwright specifications, and LazyCue fixtures.

Product infrastructure is not replaced merely because Dago owns agent execution.
It is rebuilt only where its implementation depends on a legacy model/tool state
machine.

## Completion evidence

Completion requires all of the following:

1. `make drift` proves the original test corpus is unchanged.
2. Every original Go package test passes, including race runs and Linux-only cases.
3. Every original UI unit test, Playwright specification, and LazyCue scenario passes.
4. Provider integration suites pass against the native Dago provider implementations.
5. Browser tests prove approvals, continuation, cancellation, tool ordering, streaming,
   OAuth, and Luna behavior through the real application.
6. The executable dependency graph contains no `dagoruntime` package and no reachable
   legacy Shelley model/tool loop.
