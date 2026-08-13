# Reusable browser WebAssembly packages

`browser` contains browser runtime pieces that are independent of any one agent
application.

| Package | Environment | Compatibility contract |
|---|---|---|
| `browser/browserfs` | Go `js/wasm` | Implements `dabackend.Backend` at `/workspace`; File System Access API directories are indexed without reading bodies; virtual records use `metadata`, `get`, `put`, and `delete` operations. |
| `browser/jsbridge` | Go `js/wasm` | Exposes retained global callbacks, cancellation-safe promise waits, JSON calls, and operation-store adapters. |
| `browser/justbash` | Go and `js/wasm` | Stable JSON request/response boundary with command, cwd, timeout, stdout, stderr, exit code, and optional truncation. |
| `browser/browser` | Browser TypeScript | Adapts the Go filesystem to just-bash, supplies record-oriented IndexedDB persistence, and owns the persistent just-bash mount. |

Persisted browser file records are additive JSON objects keyed by path. The
browser package does not own a database version or perform migrations; callers
provide an existing object store. Selected directory handles and file bodies
are never serialized into application snapshots.

## Wiring

A Go `js/wasm` entrypoint supplies application-specific global names while the
shared packages own the mechanics:

```go
store := jsbridge.PromiseStore{GlobalName: "agentBrowserFileStore"}
filesystem, err := browserfs.New(context.Background(), store)
shell := justbash.GlobalExecutor("agentJustBashExecute")
```

The browser worker mounts the same filesystem bridge for shell commands:

```ts
const filesystem = new WasmFileSystemAdapter({ execute, paths });
const shell = new JustBashRuntime({ filesystem });
globalThis.agentJustBashExecute = (request) => shell.executeJSON(request);
globalThis.agentBrowserFileStore = createBrowserFileStore({
  openDatabase,
  storeName: "files",
});
```

Applications retain ownership of their worker message protocol, database
schema creation, UI, and agent construction.
