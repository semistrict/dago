# Dago

Dago is an idiomatic Go implementation of the Deep Agents SDK and the focused
LangChain/LangGraph behavior it needs. It provides a provider-neutral tool loop,
middleware, required delta channels, durable checkpoints, virtual filesystems,
inline subagents, context compaction, skills, memory, and streaming without trying
to reproduce either framework in full.

The implementation targets the pinned Python releases recorded in
[`docs/upstream-manifest.json`](docs/upstream-manifest.json). The public API is
currently pre-1.0.

## Install

```sh
go get github.com/semistrict/dago
```

Dago requires Go 1.26 or newer.

## Quick start

Models implement the small `model.Chat` interface. The `model/modeltest` package
includes both finite scripted responses and Shelley's reusable prompt-driven
predictable model for tests, demos, and browser suites. This complete example uses
the finite scripted form:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func main() {
	chat := modeltest.New(model.Profile{}, modeltest.Step{
		Response: model.Response{Message: message.Assistant("Ready.")},
	})
	compiled, err := dago.New(dago.Options{
		Model:            chat,
		DisableSubagents: true,
		DisableSummary:   true,
	})
	if err != nil {
		log.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{
		Messages: []message.Message{message.Human("Introduce yourself.")},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Messages[len(result.Messages)-1].TextContent())
}
```

By default, files live in the agent’s `files` delta channel. They are isolated by
thread and become durable when a checkpoint saver is configured. Pass an explicit
filesystem, store, composite, local-shell, or remote sandbox backend when the agent
should operate elsewhere.

Declarative subagents inherit the parent model and tools unless they override them.
They receive the standard filesystem, compaction, repair, profile, and prompt-cache
stack; optional skills, permissions, structured output, and approval rules are
configured on the subagent specification. Precompiled `Runnable` subagents remain
available when the caller needs a completely custom graph. Human approval, including
approval inside a subagent, requires a checkpoint saver so the exact pending tool call
can resume without replaying completed sibling tools.

## OpenAI adapter

The focused Responses API adapter supports text and multimodal messages, tool calls,
parallel tool calls, JSON Schema structured output, token streaming, usage, prompt
caching metadata, API keys, and an explicit subscription OAuth session.

```go
chat, err := openai.NewAPIKey(os.Getenv("OPENAI_API_KEY"), openai.Options{
	Model: "gpt-5",
	ContextWindow: 128_000,
})
```

The core package never discovers credentials or chooses a provider. OAuth token
persistence is opt-in and writes only to the caller-provided private file.

## Durable execution

```go
saver, err := sqlite.Open("agent-checkpoints.sqlite")
if err != nil {
	log.Fatal(err)
}
defer saver.Close()

compiled, err := dago.New(dago.Options{Model: chat, Saver: saver})
result, err := compiled.Invoke(ctx, agent.Input{
	Config: checkpoint.Config{ThreadID: "conversation-1"},
	Messages: []message.Message{message.Human("Inspect the project.")},
})
```

Agents expose checkpoint history, replay, thread fork, and thread deletion. SQLite
and PostgreSQL savers match the pinned Python table layouts and delta-snapshot rules.
Cross-language payload compatibility is intentionally limited to the safe plain-data
subset in [`docs/SERIALIZATION.md`](docs/SERIALIZATION.md); Python-specific object
records are rejected with typed context instead of reconstructed.

## Packages

| Package | Purpose |
|---|---|
| `dago` | Deep Agent constructor and filesystem, subagent, summary, skill, memory, profile, and rubric middleware |
| `agent` | Provider-neutral model/tool graph, middleware lifecycle, approval, retry, todo, streaming, and checkpoint operations |
| `message`, `model`, `tool`, `state` | Stable public contracts and reducers |
| `model/modeltest` | Scripted and prompt-driven predictable model doubles for offline tests and examples |
| `backend` | State, memory, host filesystem, namespaced store, composite, and explicit local-shell backends |
| `backend/langsmith` | Adapter for an existing LangSmith sandbox using `langsmith-go` |
| `checkpoint` | Saver contract and in-memory implementation |
| `checkpoint/sqlite`, `checkpoint/postgres` | Python-schema-compatible durable savers |
| `store`, `store/sqlite`, `cache` | Namespaced data store and cache contracts and implementations |
| `providers/openai` | Focused Responses API adapter and credential flows |

The graph runtime is internal. Dago claims compatibility only for the Deep Agents
surface documented in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md), not general
LangChain or LangGraph compatibility.

## Examples

- [`examples/basic`](examples/basic) is a network-free invocation.
- [`examples/openai`](examples/openai) streams a live workspace summary with an API
  key.
- [`examples/shelley`](examples/shelley) is the complete imported Shelley
  application, retained as an end-to-end integration example and behavioral suite
  while its agent runtime is ported to Dago.

## Verification

```sh
make check
make checkpoint-interop
```

`make check` runs formatting, generated-fixture drift, upstream-pin validation, vet,
the deterministic suite, and race tests. PostgreSQL integration tests additionally
require `DAGO_POSTGRES_TEST_DSN`. Cross-language SQLite fixtures require `uv` and the
pinned Python packages resolved by the interop script.

## Security

Shell execution is never granted by a plain backend. `backend.LocalShell` runs trusted
host processes and is not isolation. Filesystem permissions are code-enforced and
cannot constrain shell commands, so an application must use an isolated sandbox or
omit `execute` when path-level permissions are required. See
[`docs/SECURITY.md`](docs/SECURITY.md) before exposing an agent or the web example.

The project is MIT licensed. See [`NOTICE`](NOTICE) for reference-project attribution.
