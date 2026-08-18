---
type: Engineering Playbook
title: Development operations, testing, and source map
description: Practical commands and ownership rules for dago contributors.
tags: [operations, testing, generators, ci, source-map]
---

# Development operations, testing, and source map

## Root-module workflow

Start with the smallest owning package, add `-race` for concurrent state or transport
work, and use `go vet` before the repository-wide gate:

```sh
go test ./affected/package
go test -race ./affected/package
go vet ./affected/package
make check
```

`make check` expands to formatting validation, generated conformance drift, optional
upstream revision validation, vet, deterministic tests, and `go test -race ./...`.
Use `go mod tidy -diff` and `git diff --check` when dependencies or broad edits are
involved.

## Generated and separate checks

| Surface | Command | Notes |
| --- | --- | --- |
| Root conformance | `make generate`, then `make drift` | Edit generator inputs, not generated fixtures. |
| Checkpoint interoperability | `make checkpoint-interop` | Uses the pinned Python dependency environment through the project script. |
| TinyGo closure | `make tinygo` | Compiles the basic example for native and WASM targets. |
| Terminal browser UI | `make dacode-e2e` | Uses pnpm and Playwright; every terminal-visible feature needs coverage. |
| Live OpenAI Responses | `make test-openai-live` | Opt-in; reads an existing caller-selected OAuth file and makes real requests. |
| Live OpenRouter | `make openrouter-e2e` | Opt-in; requires the provider key in the process environment. |
| Shelley Go module | `go vet ./...`, `go test ./...`, `go test -race ./...` from `examples/shelley` | Separate `go.mod`. |
| Shelley UI | pnpm type checks, lint, tests, build, and Playwright from `examples/shelley/ui` | Never use npm in this repository. |

## Test ownership

- Model-agnostic runtime semantics belong in `dagent` or `internal/graph` tests.
- Middleware, filesystem, skills, memory, and subagent composition belong in root package tests.
- Persistence contracts belong beside `dacheckpoint` or `dastore` and each concrete adapter.
- Provider protocol behavior belongs in its provider package with network-free fixtures; real calls remain opt-in.
- ACP lifecycle/projection behavior belongs in `daacp`; terminal-specific ACP construction belongs in `internal/dacode`.
- Terminal state transitions need Go tests, and user-visible paths also need the full Playwright terminal suite.
- Remote sandbox packages use caller-owned fake transports for deterministic bounds, cancellation, cleanup, and mapping tests.
- Evaluation reports must distinguish infrastructure/runtime errors from behavioral failures.

## Source map

```text
Public API policy                    docs/ARCHITECTURE.md
Upstream provenance                 docs/UPSTREAM.md, docs/upstream-manifest.json
Compatibility and security          docs/COMPATIBILITY.md, docs/SECURITY.md
Agent assembly                      dago.go, options.go
Generic runtime API                 dagent/
Graph scheduling and snapshots      internal/graph/
Messages/models/tools/state         damessage/, damodel/, datool/, dastate/
Backends and sandboxes              dabackend/
Providers                           daproviders/
Terminal agent                      internal/dacode/, cmd/dacode/
Development server                  daserver/, internal/dadev/, cmd/dago/
Evaluations                         daeval/, docs/EVALUATIONS.md
Browser example                     examples/shelley/
CI and packaged action              .github/workflows/, action.yml, ACTION.md
```

## Operational safety

Do not read or print credential files, `.env` contents, checkpoint databases, or user
state while documenting or testing. Use fake servers/transports in the normal suite.
Live checks are explicit, credential-bearing, potentially costly operations. Any cloud
resource created for an ad hoc check must be deleted and verified gone before handoff.
