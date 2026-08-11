# Repository development guide

Keep `README.md` focused on people installing and using dago. Contributor workflow,
upstream-port maintenance, generators, and verification commands belong here.

## Architecture and upstream contracts

dago is a focused Deep Agents implementation, not a general LangChain or LangGraph
port. Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) before changing public
contracts and [`docs/UPSTREAM.md`](docs/UPSTREAM.md) before changing ported behavior.
Shelley-specific port rules and completion criteria live in
[`docs/SHELLEY_NATIVE.md`](docs/SHELLEY_NATIVE.md).

Normative source revisions are pinned in
[`docs/upstream-manifest.json`](docs/upstream-manifest.json). Optional local reference
checkouts are selected with the environment variables documented in
[`docs/UPSTREAM.md`](docs/UPSTREAM.md); never embed machine-specific checkout paths.

The repository contains two Go modules:

- The root module contains the dago library, adapters, conformance fixtures, and
  small examples.
- `examples/shelley` is the end-to-end application module.
- `examples/shelley/ui` is Shelley's TypeScript/Vue application. Use `pnpm` for all
  dependency and script operations.

## Generated files

Do not edit generated files directly. Change their source and run the owning
generator:

- Root conformance fixtures: `make generate`; verify with `make drift`.
- Shelley Go outputs, including SQL queries and string forms: run `go generate ./...`
  from `examples/shelley`.
- Shelley TypeScript API types: run `pnpm generate-types` from
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

This checks formatting, generated-fixture drift, configured upstream revisions, vet,
the deterministic suite, and race tests.

Checkpoint interoperability is a separate dependency-bearing check:

```sh
make checkpoint-interop
```

It requires `uv` and resolves the pinned Python packages through the interop script.
Live PostgreSQL integration tests require `DAGO_POSTGRES_TEST_DSN`.

For Shelley, install UI dependencies first and run the Go and UI checks from their
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

Run browser coverage from `examples/shelley/ui`:

```sh
pnpm exec playwright install chromium
pnpm test:e2e
pnpm test:e2e:wasm
```

Use focused tests while iterating, then run the owning module's full checks before
publishing a change.
