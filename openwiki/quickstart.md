---
type: Codebase Guide
title: dago maintainer quickstart
description: Entry point for contributors working on the Go agent harness, adapters, terminal agent, and browser example.
tags: [go, agents, architecture, testing]
---

# dago maintainer quickstart

dago is a focused Go implementation of the Deep Agents harness. It composes models,
middleware, backends, persistence, tools, and subagents around its own internal graph
runtime; it is not a general LangChain or LangGraph port. The product README is the
installation guide. This wiki is a source-grounded maintainer map.

## Start here

- Read [Runtime and package architecture](architecture/overview.md) before changing public contracts, graph assembly, state, middleware, or backends.
- Read [Terminal coding-agent workflow](workflows/terminal-agent.md) before changing `cmd/dacode`, approvals, sessions, headless execution, ACP, MCP, or terminal rendering.
- Read [Evaluation and delivery workflow](workflows/evaluation-and-delivery.md) before changing deterministic evaluations, Harbor adapters, action packaging, or CI gates.
- Use [Operations and testing](engineering/operations-and-testing.md) for generators, focused checks, race coverage, live-test gates, and the second application module.

## Repository shape

| Area | Responsibility | First source anchor |
| --- | --- | --- |
| root `dago` package | Public agent configuration, profiles, filesystem, skills, memory, summarization, and subagent assembly. | `dago.go`, `options.go` |
| `dagent` + `internal/graph` | Public invocation/stream/state API over the internal checkpointed graph runtime. | `dagent/agent.go`, `internal/graph/graph.go` |
| `damodel`, `damessage`, `datool`, `dastate` | Provider-neutral model, message, tool, and state contracts. | package documentation and tests |
| `dacheckpoint`, `dastore`, `dacache` | Durable thread state, cross-thread storage, and cache contracts plus concrete adapters. | package roots and adapter subpackages |
| `dabackend` | Filesystem/execution boundary, local and remote sandbox adapters, and confinement rules. | `dabackend/backend.go`, `docs/SECURITY.md` |
| `daproviders/*` | Provider-specific model clients and profile construction. | provider package roots |
| `daacp`, `daserver`, `daagentprotocol` | ACP editor adapter, local development server, and remote background-agent protocol client. | package roots |
| `daeval` | Provider-neutral behavioral evaluation, Harbor integration, benchmark adapters, and scorecards. | `docs/EVALUATIONS.md` |
| `internal/dacode` | Terminal application, session runner, approvals, headless path, browser terminal, and UI tests. | `internal/dacode/run.go`, `internal/dacode/app.go` |
| `examples/shelley` | Separate Go application module and TypeScript/Vue browser UI. | `examples/shelley/go.mod`, `examples/shelley/ui/package.json` |

## Fast local loop

From the repository root:

```sh
go test ./path/to/changed/package
go test -race ./path/to/changed/package
go vet ./path/to/changed/package
git diff --check
```

Before handing off a root-module change, run `make check`. It checks formatting,
generated conformance drift, configured upstream revisions, vet, deterministic tests,
and the race suite. Checkpoint interoperability, live provider tests, TinyGo, the
terminal browser suite, and the Shelley module have separate commands documented in
[Operations and testing](engineering/operations-and-testing.md).

## Boundaries that shape changes

- Mandatory public dependencies are positional. Optional zero values should be useful, and constructors normally establish invariants without returning an error.
- Provider SDK types stay out of core contracts. Concrete network, database, sandbox, and credential behavior belongs in adapters.
- Model output and repository content are untrusted data. Authority is enforced by tools, middleware, approvals, rooted filesystems, and sandbox transports.
- Checkpoint, stream, and report envelopes are versioned. Unknown versions fail explicitly; persisted semantic changes require migration planning.
- Root conformance outputs and Shelley generated files must be changed through their owning generators.
- Tests encode intended behavior. A reproduction that passes only while a bug remains does not belong in the green suite.
