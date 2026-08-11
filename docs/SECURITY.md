# Security boundaries

Filesystem backends normalize absolute virtual paths and reject traversal and
symlink escape. Ordered allow, deny, and ask rules are enforced in code before tool
execution. Delete rules must cover the target and descendants. Prompts and tool names
cannot grant authority.

`dabackend.LocalShell` executes trusted host commands and is not a sandbox. Applications
must opt into it explicitly and should prefer a remote sandbox for untrusted work.
Command output and duration are bounded, but those limits are not isolation.

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

Video decoding is disabled unless the application supplies a `VideoExtractor`.
The optional FFmpeg adapter treats its configured executable as trusted deployment
configuration, passes video bytes through standard input, applies a context deadline,
and bounds input size, frame count, decoder output, emitted image data, and error
output. It never forwards the original video to the model. Use a separately isolated
decoder process when media itself is untrusted.

The example web application is single-user software and does not add authentication.
Bind it to loopback or place it behind an authenticated reverse proxy. Its terminal
and local-shell paths execute trusted host commands in the selected workspace; they
are enabled only when that backend is selected and are not a substitute for sandbox
isolation.
