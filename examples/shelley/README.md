# Shelley example

Shelley is a browser-based, multi-conversation coding-agent example built on
dago. This subtree was copied from and modified from the upstream Shelley
application; see [`UPSTREAM.md`](UPSTREAM.md) and [`LICENSE`](LICENSE).

## Run locally

Install the UI dependencies with pnpm, then build and run the native server:

```sh
cd examples/shelley/ui
pnpm install --frozen-lockfile
cd ..
make serve
```

The server listens on loopback by default. It has no multi-user authorization
or process sandbox: do not expose it to an untrusted network, and do not enable
host shell tools outside an appropriately isolated environment.

Repository guidance files and repository skills are disabled by default. After
reviewing a workspace, opt in with `serve --trust-workspace-guidance`.

## Run in a browser only

```sh
make wasm-serve
```

The browser build prompts once for an OpenAI API key and keeps it in the Web
Worker's memory. Reloading the page clears the key. Files and shell commands use
the browser-local virtual filesystem and do not access the host.

## Verify

```sh
make test
```

The UI directory also provides `pnpm test`, `pnpm typecheck`,
`pnpm test:e2e`, and `pnpm test:e2e:wasm`.

The browser artifact includes a generated CycloneDX SBOM and third-party
notices alongside the application files.
