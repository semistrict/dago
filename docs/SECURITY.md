# Security boundaries

The local filesystem backend performs confined operations through `os.Root`.
Traversal and symlink escapes are rejected by the rooted operating-system API rather
than path-prefix checks. Ordered allow, deny, and ask rules are enforced in code
before tool execution. Delete rules must cover the target and descendants. Prompts
and tool names cannot grant authority.

`dabackend.LocalShell` executes trusted host commands and is not a sandbox. Applications
must opt into it explicitly and should prefer Docker or a remote sandbox for untrusted
work. Its inherited file methods use `os.Root`, but `os.Root` cannot confine an
arbitrary subprocess: shell commands may use absolute paths or leave the working
directory. Command output and duration are bounded, but those limits are not isolation.

`dabackend/docker` creates and owns a container from an explicitly selected local
image. Its defaults disable networking, drop all Linux capabilities, enable
`no-new-privileges`, make the root filesystem read-only, limit memory (without
additional swap), CPU, and PIDs, and mount only its dedicated workspace. Timeout or
cancellation restarts the container
to ensure the command is terminated while preserving workspace files. Docker remains
a host-privileged isolation boundary: daemon configuration, image provenance, kernel
security, and any explicitly relaxed options remain the operator's responsibility.
The adapter never mounts the Docker socket and never pulls an image implicitly.

The safe checkpoint serializer is an allowlist. It does not implement pickle,
dynamic imports, arbitrary constructors, or callable execution. OAuth tokens are
stored only when the caller provides a path, using an atomic private file with mode
0600. Error bodies are bounded and credentials are not logged.

The subscription OAuth helper uses PKCE, verifies the callback state, binds its
temporary callback listener to loopback, honors cancellation, refreshes expired
credentials, and stores tokens only in its own caller-selected file. It does not read
or reuse another application’s credential store. API-key and OAuth credentials remain
adapter concerns and are never placed into graph state or checkpoints.

The Agent Protocol client sends credentials only to its configured origin, refuses
redirects, bounds response bodies, and escapes thread/run identifiers. Its automatic
API-key lookup follows the upstream precedence (`LANGGRAPH_API_KEY`, then
`LANGSMITH_API_KEY`, then `LANGCHAIN_API_KEY`); use a custom runner when a different
credential policy is required.

`dago dev` is an unauthenticated local development server. It binds to loopback by
default, allows the hosted Studio origin and loopback browser origins, and places its
generated wrapper plus SQLite state under `.dago_api`. Do not bind it to a shared or
public interface. Environment values are passed to the compiled application process;
they are not returned by the Agent Server API, but application code and model tools
still have whatever access the host process grants them.

Video decoding is disabled unless the application supplies a `VideoExtractor`.
The optional FFmpeg adapter treats its configured executable as trusted deployment
configuration, passes video bytes through standard input, applies a context deadline,
and bounds input size, frame count, decoder output, emitted image data, and error
output. It never forwards the original video to the model. Use a separately isolated
decoder process when media itself is untrusted.

The example web application is single-user software and does not add authentication.
Its native server refuses non-loopback TCP listeners. An authenticated reverse proxy
may connect to that loopback listener; `--require-header` is only a defense-in-depth
proxy integration and is safe only when the proxy removes caller-supplied copies of
the header. Terminal and local-shell paths execute trusted host commands and are not
a substitute for sandbox isolation.

Repository guidance and repository skills are ignored by default because a checkout
may be unreviewed. `serve --trust-workspace-guidance` opts into those prompt inputs
after the operator has reviewed them; user-installed and built-in skills remain
available without that flag. Tool authorization is enforced independently of prompt
content.

The browser-only example stores its provider credential in `localStorage` and restores
it into Web Worker memory after reload. The credential, conversations, and
virtual-workspace files are readable to scripts on the deployment origin, so deploy it
on a dedicated origin and treat every same-origin script as trusted. The published
entry point includes a Content Security Policy, but that policy is defense in depth
rather than storage encryption or origin isolation.
