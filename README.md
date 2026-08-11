# dago

dago is an idiomatic Go implementation of the Deep Agents SDK and the focused
LangChain/LangGraph behavior it needs. It provides a provider-neutral tool loop,
middleware, required delta channels, durable checkpoints, virtual filesystems,
inline and background subagents, context compaction, skills, memory, and streaming without trying
to reproduce either framework in full.

The implementation targets the pinned Python releases recorded in
[`docs/upstream-manifest.json`](docs/upstream-manifest.json). The public API is
currently pre-1.0.

## Install

```sh
go get github.com/semistrict/dago
```

dago requires Go 1.26 or newer.

## Quick start

Models implement the small `damodel.Chat` interface. The `damodel/modeltest` package
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
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func main() {
	chat := modeltest.New(damodel.Profile{}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("Ready.")},
	})
	compiled, err := dago.New(dago.Options{
		Model:            chat,
		DisableSubagents: true,
		DisableSummary:   true,
	})
	if err != nil {
		log.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("Introduce yourself.")},
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

Binary media is returned opaquely by default. Supplying `VideoExtractor` in
`dago.Options` changes video `read_file` pagination to seconds and returns sampled
JPEG frames. `NewFFmpegVideoExtractor` is the optional ready-made implementation;
the FFmpeg executable remains an external deployment dependency.

Declarative subagents inherit the parent model and tools unless they override them.

### Type-safe tools

`datool.New` derives an object schema from the handler's input struct, validates
model arguments, and decodes them before calling the handler:

```go
type weatherInput struct {
	Location string `json:"location" description:"City and state" jsonschema:"minLength=1"`
	Units    string `json:"units,omitempty" jsonschema:"enum=celsius|fahrenheit,default=celsius"`
}

weather, err := datool.New("weather", "Get the current weather.", func(ctx context.Context, input weatherInput) (string, error) {
	return input.Location + ": sunny", nil
})
```

Fields use `encoding/json` names and `omitempty` behavior. A `description` tag
sets field documentation; `jsonschema` supports requirements, formats, enums,
defaults, examples, string/array/object lengths, patterns, and numeric bounds.
Handlers that need call state can use `datool.RuntimeFromContext(ctx)`.
Returning a `datool.Result` preserves its full content, artifact, state update,
interrupt, and handoff; strings become text results and other values become JSON text.
Runtime schema details can be layered onto the generated schema with functional
options such as `WithPropertyType`, `WithPropertyEnum`, `WithPropertyValue`,
`WithPropertySchema`, `WithoutProperty`, or the lower-level `WithTransformSchema`.
They receive the standard filesystem, compaction, repair, profile, and prompt-cache
stack; optional skills, permissions, structured output, and approval rules are
configured on the subagent specification. Precompiled `Runnable` subagents remain
available when the caller needs a completely custom graph. Human approval, including
approval inside a subagent, requires a checkpoint saver so the exact pending tool call
can resume without replaying completed sibling tools.

Applicable built-in harness profiles resolve from the model's provider and identifier.
They include Anthropic prompt overlays and the full Nemotron 3 Ultra repair, retry,
progress-budget, tool-selection, entity-resolution, and answer-completeness stack.
Provider construction defaults for OpenAI, NVIDIA, and OpenRouter can be layered with
caller registrations through `ApplyProviderProfile`.

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
result, err := compiled.Invoke(ctx, dagent.Input{
	Config: dacheckpoint.Config{ThreadID: "conversation-1"},
	Messages: []damessage.Message{damessage.Human("Inspect the project.")},
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
| `dago` | Deep Agent constructor; filesystem and optional video extraction, inline/background subagent, summary, skill, memory, profile, and rubric middleware; Agent Protocol background client |
| `dagent` | Provider-neutral model/tool graph, middleware lifecycle, approval, retry, todo, streaming, and checkpoint operations |
| `damessage`, `damodel`, `datool`, `dastate` | Stable public contracts and reducers |
| `damodel/modeltest` | Scripted and prompt-driven predictable model doubles for offline tests and examples |
| `dabackend` | State, memory, host filesystem, namespaced store, composite, and explicit local-shell backends |
| `dabackend/langsmith` | Adapter for an existing LangSmith sandbox using `langsmith-go` |
| `dabackend/contexthub` | Persistent Context Hub agent-repository files with linked-entry and parent-commit support |
| `dacheckpoint` | Saver contract and in-memory implementation |
| `dacheckpoint/sqlite`, `dacheckpoint/postgres` | Python-schema-compatible durable savers |
| `dastore`, `dastore/sqlite`, `dacache` | Namespaced data store and cache contracts and implementations |
| `daproviders/openai` | Focused Responses API adapter and credential flows |
| `daskill` | Agent Skills parsing, discovery, validation, and rendering contracts |

The graph runtime is internal. dago claims compatibility only for the Deep Agents
surface documented in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md), not general
LangChain or LangGraph compatibility.

## Examples

- [`examples/basic`](examples/basic) is a network-free invocation.
- [`examples/openai`](examples/openai) streams a live workspace summary with an API
  key.
- [`examples/shelley`](examples/shelley) is a copied and modified Shelley
  application used as an end-to-end dago integration example.

## Verification

```sh
make check
make checkpoint-interop
```

`make check` runs formatting, generated-fixture drift, configured upstream-pin
validation, vet, the deterministic suite, and race tests. PostgreSQL integration tests additionally
require `DAGO_POSTGRES_TEST_DSN`. Cross-language SQLite fixtures require `uv` and the
pinned Python packages resolved by the interop script.

## Security

Shell execution is never granted by a plain backend. `dabackend.LocalShell` runs trusted
host processes and is not isolation. Filesystem permissions are code-enforced and
cannot constrain shell commands, so an application must use an isolated sandbox or
omit `execute` when path-level permissions are required. See
[`docs/SECURITY.md`](docs/SECURITY.md) before exposing an agent or the web example.

The core project is MIT licensed. The copied and modified Shelley example is
Apache-2.0 licensed. See [`NOTICE`](NOTICE) for attribution and additional notices.
