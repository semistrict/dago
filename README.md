# dago

dago is an idiomatic Go implementation of the Deep Agents SDK and the focused
LangChain/LangGraph behavior it needs. It provides a provider-neutral tool loop,
middleware, required delta channels, durable checkpoints, virtual filesystems,
inline and background subagents, context compaction, skills, memory, and streaming without trying
to reproduce either framework in full.

The public API is currently pre-1.0.

[Try the shelley-in-dago browser demo](https://semistrict.github.io/dago/).

## Install

```sh
go get github.com/semistrict/dago
```

dago requires Go 1.26 or newer.

### Interactive coding agent

`dacode` is a terminal coding agent with durable threads and goals, streaming tool
activity, workspace-aware instructions and skills, and review gates for file writes
and shell commands. It uses Bubble Tea, Bubbles, and Lip Gloss for its terminal
interface.

```sh
go install github.com/semistrict/dago/cmd/dacode@latest
dacode
```

Run the TUI directly without installing it:

```sh
go run github.com/semistrict/dago/cmd/dacode@latest
```

From a dago source checkout, use `go run ./cmd/dacode` instead.

On first run, `dacode` opens OpenAI subscription sign-in and stores its refreshable
session under the user configuration directory. Set `OPENAI_API_KEY` to use API-key
authentication instead; an explicit key takes precedence over the saved session.

Actions that change files or run commands are routed through a separate, read-only
reviewer model by default. Review failures return to a user decision.
`--approval-model` selects that reviewer, `--manual-review` requires a user decision
for every gated action, and `--yolo` bypasses review.

Use `-n 'task'` for one-shot operation, `-r ID` to resume a durable thread, and
`--cwd PATH` to select the workspace. Run `dacode resume` to choose a session
before opening the TUI, or `dacode resume ID` to resume a known session. Inside
the TUI, `/threads` opens the same picker. Selected sessions restore their
transcripts before continuing.

Use `/goal <objective>` for work that should continue across turns until complete or
blocked. `/goal show`, `/goal pause`, `/goal resume`, `/goal clear`, and
`/goal budget <tokens|clear>` control the persisted goal. Active goals resume
automatically when the thread becomes idle; one-shot mode follows the same lifecycle.

Use `dacode acp` (or `--acp`) to serve the coding agent to an ACP-compatible editor
over standard input and output. The editor owns the session and permission prompts
in this mode; `--yolo` remains available to bypass mutating-tool approval gates.
Each editor session gets its own workspace runner and any declared stdio, HTTP, or
SSE MCP servers. HTTP headers are forwarded for per-session MCP authentication.

`--serve-xtermjs` serves the same PTY-backed TUI on a loopback-only web address
and prints its URL. Use `--xtermjs-address HOST:PORT` to select a specific
loopback listener.

## Quick start

Models implement the small `damodel.Chat` interface. This example uses the OpenAI
adapter, but the agent and tool APIs are provider-neutral:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daproviders/openai"
)

func main() {
	chat, err := openai.NewAPIKey(os.Getenv("OPENAI_API_KEY"), openai.Options{
		Model:         "gpt-5",
		ContextWindow: 128_000,
	})
	if err != nil {
		log.Fatal(err)
	}
	compiled := dago.New(chat, dago.Options{})
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

Agent-owned facilities use that same backend automatically. Configure them as values
instead of constructing middleware with a duplicate backend argument:

```go
compiled := dago.New(chat, dago.Options{
	Backend: workspace,
	Skills: dago.Skills{Sources: []string{"/skills"}},
	Memory: dago.Memory{Sources: []string{"/AGENTS.md"}},
})
```

Durable goals are an opt-in agent facility and require a checkpoint saver. The
middleware exposes `create_goal`, `get_goal`, and constrained `update_goal` tools;
`dagoal.Service` provides host-owned pause, resume, budget, objective, and clear
operations:

```go
goalOptions := dagoal.Options{}
compiled := dago.New(chat, dago.Options{
	Middleware: []dagent.Middleware{dagoal.Middleware(goalOptions)},
	Saver:      saver,
})
goals := dagoal.NewService(compiled, goalOptions)
```

Binary media is returned opaquely by default. Supplying
`dago.Options.Filesystem.VideoExtractor` changes video `read_file` pagination to
seconds and returns sampled JPEG frames. `davideo.NewFFmpeg` is the optional
ready-made implementation; the FFmpeg executable remains an external deployment
dependency.

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

Typed adapters keep the state and checkpoint wire formats flexible without
requiring application assertions. Use `dagent.Field` with a
`dagent.FieldSpec[T]` to declare typed reducers, `datool.StateAs[T]` for tool
state, and `dagent.DepsAs[T]` or `datool.DepsAs[T]` for application dependencies
supplied through `Options.Deps`. `dagent.ResumeAs[T]` accepts both live Go values
and checkpoint-restored plain JSON values. Structured results can be declared
with `dagent.StructuredOutputFor[T]` and decoded with
`dagent.StructuredAs[T]`; the latter validates against the schema derived from T.
`damessage.MetadataAs[T]` and `damessage.SetMetadata` provide the same typed
boundary for raw JSON metadata maps.

Owned agent streams support `for event, err := range stream.Events()`. Model
streams support `for chunk, err := range stream.Chunks()`. Both iterators close
their stream on completion, error, or early loop exit; `Next` and `Close` remain
available for explicit control.
They receive the standard filesystem, compaction, repair, profile, and prompt-cache
stack; optional skills, permissions, structured output, and approval rules are
configured on the subagent specification. Precompiled `Runnable` subagents remain
available when the caller needs a completely custom graph. Human approval, including
approval inside a subagent, requires a checkpoint saver so the exact pending tool call
can resume without replaying completed sibling tools.

Applicable built-in Anthropic harness profiles resolve from the model's provider and
identifier. The full Nemotron 3 Ultra repair, retry, progress-budget, tool-selection,
entity-resolution, and answer-completeness stack is available explicitly from
`daproviders/nemotron`. Provider construction defaults for OpenAI, NVIDIA, and
OpenRouter are available as an explicit `daproviders/profile.Profiles` value.

## JavaScript interpreter

Enable a persistent, sandboxed QuickJS-ng REPL with `Interpreter`. The `js_eval`
tool supports top-level await, console output, functions and variables that persist
through checkpoints, and concurrent programmatic tool calls. It runs the exact
`quickjs-rs` 0.2.5 WASM guest under Wazero's interpreter backend, including in Go
browser-WASM builds. TinyGo builds exclude the Wazero-backed implementation;
enabling `Interpreter` in a TinyGo build fails during agent construction, and the
Shelley TinyGo application omits `js_eval` from its tool catalog.

```go
compiled := dago.New(chat, dago.Options{
	Saver: saver,
	Interpreter: dago.Interpreter{
		Enabled: true,
		PTC: []string{"read_file", "glob", "grep", "search"},
	},
})
```

The first memory image is checkpointed as an anchor. The generated QuickJS guest
uses WAFL-style 4 KiB write barriers, so subsequent checkpoints copy only dirty
memory pages. A nil `PTC` allowlist exposes only `read_file`, `glob`, and
`grep`; an empty non-nil list disables PTC. Explicitly allowlisted tools execute
inside `js_eval` and do not pass through model-tool approval middleware, so mutating
tools should only be included when that direct authority is intended.

## OpenAI adapter

The focused Responses API adapter supports text and multimodal messages, tool calls,
parallel tool calls, JSON Schema structured output, token streaming, usage, prompt
caching metadata, API keys, and an explicit subscription OAuth session. Standard
OpenAI endpoints use persistent Responses WebSocket connections by default and send
incremental input on compatible successive turns. Set `ResponsesWebSocket` to
`new(false)` to force HTTP; compatible custom `BaseURL` endpoints can opt in with
`new(true)`. Standard endpoints also enable remote server-side compaction by default:
at 90% of `ContextWindow` (or 200,000 tokens when it is unknown), the adapter sends a
compaction-trigger Responses request, preserves its encrypted state, and resumes the
turn. Set `ServerCompaction` to `new(false)` to disable it or set
`CompactionThreshold` to override the trigger point.

```go
chat, err := openai.NewAPIKey(os.Getenv("OPENAI_API_KEY"), openai.Options{
	Model: "gpt-5",
	ContextWindow: 128_000,
})
```

The core package never discovers credentials or chooses a provider. OAuth token
persistence is opt-in and writes only to the caller-provided private file.

## OpenRouter adapter

The OpenRouter adapter uses OpenRouter's Responses API and preserves the same
text, multimodal, tool-calling, structured-output, reasoning, web-search, usage,
and streaming contracts as the OpenAI adapter. It also supports optional app
attribution and provider routing:

```go
chat, err := openrouter.New(os.Getenv("OPENROUTER_API_KEY"), openrouter.Options{
	Model:    "anthropic/claude-sonnet-4.6",
	AppURL:   "https://example.com/my-agent",
	AppTitle: "My Agent",
	Routing: &openrouter.ProviderRouting{
		Ignore:         []string{"azure"},
		DataCollection: "deny",
	},
	ContextWindow: 200_000,
})
```

`BaseURL` defaults to `https://openrouter.ai/api/v1`. Credentials remain
explicit; the adapter does not read environment variables itself.

## Durable execution

```go
saver, err := sqlite.Open("agent-checkpoints.sqlite")
if err != nil {
	log.Fatal(err)
}
defer saver.Close()

compiled := dago.New(chat, dago.Options{Saver: saver})
result, err := compiled.Invoke(ctx, dagent.Input{
	Config: dacheckpoint.Config{ThreadID: "conversation-1"},
	Messages: []damessage.Message{damessage.Human("Inspect the project.")},
})
```

Agents expose checkpoint history, replay, thread fork, and thread deletion. SQLite
and PostgreSQL savers match the supported Python table layouts and delta-snapshot rules.
Cross-language payload compatibility is intentionally limited to the safe plain-data
subset in [`docs/SERIALIZATION.md`](docs/SERIALIZATION.md); Python-specific object
records are rejected with typed context instead of reconstructed.

## Agent Client Protocol

`daacp` exposes an agent to ACP-compatible editors over newline-delimited JSON-RPC.
The process must reserve standard output for protocol messages; send logs to standard
error.

```go
server := daacp.New(compiled, daacp.Options{
	Name:    "workspace-agent",
	Version: "1.0.0",
})
if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
	log.Fatal(err)
}
```

The adapter supports ACP v1 session creation and durable loading with replay-marked
history, prompts, cancellation, close, authentication handshakes, session
configuration negotiation, streamed text and reasoning, tool status and progress,
plans, and approve/reject permission requests. A session factory can construct an
isolated runner from the requested working directory and MCP declarations; stdio,
HTTP, and SSE MCP transports are supported by `dacode`. The session working
directory is also available to tools as `daacp.ConfigurableCWD`. Additional roots,
client filesystem/terminal delegation, and ACP-routed MCP transport are not
advertised.

## LangSmith Studio

`dago dev` exposes configured Go agent factories through the LangGraph Agent
Server protocol, persists development threads and store values in SQLite, watches
Go/module/config/environment files, and rebuilds the server when they change.

```sh
go install github.com/semistrict/dago/cmd/dago@latest
```

Export a factory that accepts the server-owned runtime. Using its saver and store
is required for Studio state, history, replay, and thread operations to address the
same durable data as agent runs:

```go
func NewAgent(_ context.Context, runtime daserver.Runtime) (*dagent.Agent, error) {
	return dago.New(chat, dago.Options{
		Saver: runtime.Saver,
		Store: runtime.Store,
		Deps:  runtime.Deps,
	}), nil
}
```

Point `dago.json` at the package and exported factory:

```json
{
  "graphs": {
    "agent": {
      "path": "./agent:NewAgent",
      "description": "Workspace agent"
    }
  },
  "env": ".env"
}
```

Then start the API and Studio:

```sh
dago dev
```

The default API is `http://localhost:2024`; `--no-browser`, `--host`, `--port`,
`--config`, and `--n-jobs-per-worker` are available. Development data and the
generated wrapper stay under the ignored `.dago_api` directory. The
[`examples/studio`](examples/studio) configuration is a network-free smoke test:

```sh
dago dev -c examples/studio/dago.json
```

This is a focused local Studio integration, not a claim that arbitrary LangGraph
applications can run in Go. Its supported Agent Server resources and current
limits are listed in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md).

## Packages

| Package | Purpose |
|---|---|
| `dago` | Deep Agent constructor plus filesystem, JavaScript interpreter, subagent, summary, skill, memory, profile, and rubric middleware |
| `dagent` | Provider-neutral model/tool graph, middleware lifecycle, approval, retry, todo, streaming, and checkpoint operations |
| `dagoal` | Durable goal state, model tools, host lifecycle controls, accounting, and continuation messages |
| `damessage`, `damodel`, `datool`, `dastate` | Stable public contracts and reducers |
| `damodel/modeltest` | Scripted and prompt-driven predictable model doubles for offline tests and examples |
| `dabackend` | State, memory, host filesystem, namespaced store, composite, and explicit local-shell backends |
| `dabackend/docker` | Owned local Docker sandbox with a confined workspace, hardened defaults, resource limits, and lifecycle cleanup |
| `dabackend/langsmith` | Adapter for an existing LangSmith sandbox using `langsmith-go` |
| `dabackend/contexthub` | Persistent Context Hub agent-repository files with linked-entry and parent-commit support |
| `dacheckpoint` | Saver contract and in-memory implementation |
| `browser/...` | Reusable browser WebAssembly filesystem, IndexedDB checkpoint, promise bridge, just-bash, directory-handle, and WebGPU adapters; see [`browser/README.md`](browser/README.md) |
| `dacheckpoint/sqlite`, `dacheckpoint/postgres` | Python-schema-compatible durable savers |
| `dastore`, `dastore/sqlite`, `dacache` | Namespaced data store and cache contracts and implementations |
| `daproviders/openai` | Focused Responses API adapter and credential flows |
| `daproviders/nemotron` | Opt-in Nemotron harness profiles and model-output repair middleware |
| `daproviders/profile` | Explicit provider-construction profile sets and built-in defaults |
| `davideo` | Video extraction contracts and optional bounded FFmpeg adapter |
| `daacp` | Agent Client Protocol v1 server adapter for editor integration |
| `daagentprotocol` | Agent Protocol background-subagent client |
| `daprofilecfg` | JSON/YAML-safe harness-profile configuration |
| `daskill` | Agent Skills parsing, discovery, validation, and rendering contracts |
| `daworkspace` | Shared workspace-instruction discovery, scoped guidance summaries, and conventional directory filtering |
| `daserver` | Embeddable LangGraph Agent Server protocol for LangSmith Studio and SDK clients |

The graph runtime is internal. dago claims compatibility only for the Deep Agents
surface documented in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md), not general
LangChain or LangGraph compatibility.

## Docker sandbox

`dabackend/docker` creates and owns a container from an image that is already
available to the local Docker daemon. It never pulls an image implicitly. The
image must provide `/bin/sh` and `sleep`.

```go
sandbox, err := docker.New(ctx, docker.Options{
	Image: "my-agent-sandbox:local",
})
if err != nil {
	log.Fatal(err)
}
defer sandbox.Close()

compiled := dago.New(chat, dago.Options{
	Backend: sandbox,
})
```

By default the container has no network, a read-only root filesystem, no Linux
capabilities, `no-new-privileges`, bounded memory/CPU/PIDs, a writable `/tmp`, and
one private workspace bind mount. Set `Network`, resource fields, `User`, or
`WritableRoot` explicitly when the workload requires broader authority. Closing
the backend forcibly removes its container and its automatically created workspace.

## Examples

- [`examples/basic`](examples/basic) is a network-free invocation.
- [`examples/openai`](examples/openai) streams a live workspace summary with an API
  key.
- [`examples/studio`](examples/studio) runs a network-free agent through `dago dev`
  and LangSmith Studio.
- [`examples/shelley`](examples/shelley) contains shelley-in-dago, a full web
  application powered by dago.
  [Run it in your browser](https://semistrict.github.io/dago/).

## Security

Shell execution is never granted by a plain backend. `dabackend.LocalShell` runs trusted
host processes and is not isolation. Filesystem permissions are code-enforced and
cannot constrain shell commands, so an application must use an isolated sandbox or
omit `execute` when path-level permissions are required. See
[`docs/SECURITY.md`](docs/SECURITY.md) before exposing an agent or the web example.

The core project is MIT licensed. The copied and modified shelley-in-dago example is
Apache-2.0 licensed. See [`NOTICE`](NOTICE) for attribution and additional notices.
