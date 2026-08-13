# Browser WASM support

This package contains the JavaScript-side adapters used by Go WebAssembly
agents in browser workers:

- `filesystem` adapts the Go filesystem operation bridge to just-bash's
  `IFileSystem` without copying a workspace snapshot.
- `checkpoint` implements normalized IndexedDB checkpoint operations for the
  Go `browser/checkpoint` saver.
- `directory-handle` stores selected directory handles and manages read/write
  permissions using caller-supplied database and picker names.
- `indexeddb` persists virtual files as independent records and reads metadata
  without loading file bodies.
- `just-bash` mounts the shared filesystem and executes bounded shell requests.
- `webgpu-qwen` provides the current Qwen WebGPU model loader and message/tool
  conversion used by the reusable Go WebGPU adapter.
- `esbuild` provides the browser zlib shim required by just-bash 3.2.

The consumer owns its database schema and global bridge names. This package
does not create migrations or application-specific stores.
