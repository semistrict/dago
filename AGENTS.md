# Repository development guide

Keep `README.md` focused on people installing and using dago. Contributor workflow,
upstream-port maintenance, generators, and verification commands belong here.

## Architecture and upstream contracts

dago is a focused Deep Agents implementation, not a general LangChain or LangGraph
port. Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) before changing public
contracts and [`docs/UPSTREAM.md`](docs/UPSTREAM.md) before changing ported behavior.
shelley-in-dago-specific port rules and completion criteria live in
[`docs/SHELLEY_IN_DAGO.md`](docs/SHELLEY_IN_DAGO.md).

Normative source revisions are pinned in
[`docs/upstream-manifest.json`](docs/upstream-manifest.json). Optional local reference
checkouts are selected with the environment variables documented in
[`docs/UPSTREAM.md`](docs/UPSTREAM.md); never embed machine-specific checkout paths.

Constructors must not return errors solely for static input validation. Put required
dependencies and values in positional parameters rather than option structs, and
panic when a runtime check is truly necessary to reject a programmer mistake. Keep
constructor errors for external configuration, I/O, remote operations, and runtime
setup failures that callers can meaningfully handle.

Constructor option structs are optional zero-or-one variadic tail parameters, such
as `options ...Options`; resolve them with `internal/optionvalue`. Do not put required
values in those structs.

The repository contains two Go modules:

- The root module contains the dago library, adapters, conformance fixtures, and
  small examples.
- `examples/shelley` is the shelley-in-dago end-to-end application module.
- `examples/shelley/ui` is the shelley-in-dago TypeScript/Vue application. Use `pnpm` for all
  dependency and script operations.

## Generated files

Do not edit generated files directly. Change their source and run the owning
generator:

- Root conformance fixtures: `make generate`; verify with `make drift`.
- shelley-in-dago Go outputs, including SQL queries and string forms: run `go generate ./...`
  from `examples/shelley`.
- shelley-in-dago TypeScript API types: run `pnpm generate-types` from
  `examples/shelley/ui`.

Commit source and regenerated outputs together. `make check` rejects stale root
fixtures.

## Tests and verification

Tests must assert intended behavior. A temporary reproduction must either fail while
the bug exists or stay outside the passing suite until its expectation is changed to
the correct behavior.

Run the complete root checks from the repository root:

```sh
make check
```

The gated live Responses WebSocket test reads an existing subscription OAuth
JSON file and calls the real API. It verifies warmup, streaming, tool calls,
incremental continuation, remote compaction and state replay, unrelated-history
reset, cancellation, and reconnect:

```sh
DAGO_OPENAI_OAUTH_FILE=/path/to/auth.json make test-openai-live
```

Set `DAGO_OPENAI_LIVE_MODEL` to override the default live model.

This checks formatting, generated-fixture drift, configured upstream revisions, vet,
the deterministic suite, and race tests.

Checkpoint interoperability is a separate dependency-bearing check:

```sh
make checkpoint-interop
```

It requires `uv` and resolves the pinned Python packages through the interop script.
Live PostgreSQL integration tests require `DAGO_POSTGRES_TEST_DSN`.

Live OpenRouter provider coverage is opt-in and reads the API key only from the
process environment:

```sh
OPENROUTER_API_KEY=... make openrouter-e2e
```

The suite uses `deepseek/deepseek-v4-flash-0731` by default. Set
`DAGO_OPENROUTER_E2E_MODEL` to exercise another Responses-compatible model.
The same target also verifies that Shelley's custom-model builder can call the
default model with a forced tool request.

For shelley-in-dago, install UI dependencies first and run the Go and UI checks from their
respective module directories:

```sh
cd examples/shelley/ui
pnpm install --frozen-lockfile
pnpm type-check
pnpm type-check:vue
pnpm lint
pnpm test
pnpm build

cd ..
go vet ./...
go test ./...
go test -race ./...
```

Run shelley-in-dago browser coverage from `examples/shelley/ui`:

```sh
pnpm exec playwright install chromium
pnpm test:e2e
pnpm test:e2e:wasm
```

Compile the core library closure and Shelley browser application with the
pinned TinyGo release:

```sh
make tinygo
make -C examples/shelley wasm-tinygo-check
make -C examples/shelley wasm-tinygo
```

`wasm-tinygo` applies checksum-verified upstream development fixes that are
not yet in TinyGo 0.41.1. Remove the backports when updating the pinned release
to one that contains both fixes.

Run the dacode PTY browser coverage from `internal/dacode/xtermjs`:

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium
pnpm test:e2e
```

Use focused tests while iterating, then run the owning module's full checks before
publishing a change.

## Studio development server

The `dago dev` supervisor owns generated wrapper source, binaries, and local SQLite
state under `.dago_api`; never edit those outputs. The user configuration is
`dago.json`, and graph entries use `package-path:ExportedFactory`. Factories accept
`daserver.Runtime` and must pass its saver and store into their agent.

Exercise the network-free configuration with:

```sh
go run ./cmd/dago dev -c examples/studio/dago.json --no-browser
```

Protocol behavior belongs in `daserver` handler tests. Config resolution,
wrapper generation, environment overlays, and supervision behavior belong in
`internal/dadev` tests.
