# Project MCP trust

`DiscoverConfigs` resolves the standard `.mcp.json` layers from lowest to
highest precedence: `~/.deepagents/.mcp.json`,
`<project>/.deepagents/.mcp.json`, then `<project>/.mcp.json`. A caller-selected
explicit path bypasses those defaults. Later definitions shadow same-name earlier
definitions. Files and definitions are bounded, links and special files are refused,
and optional broken sources produce content-free diagnostics while an explicit broken
source fails.

Keep each project `ConfiguredServer.Definition` raw until after the trust decision
below. Once allowed, `ResolveConnection` validates the transport and expands only
`${VAR}` and `${VAR:-default}` through the required process-environment lookup. It
supports stdio, HTTP, SSE, header and child-environment values, OAuth declarations,
and allow/deny tool patterns without mutating or persisting resolved credentials.

`damcp` is the UI-neutral trust boundary for MCP servers discovered in a project.
It does not connect to a server. A host parses the project's MCP definitions into
`Server` values, loads the operator's user-level policy, and calls `Policy.Resolve`
before constructing any transport.

```go
store := damcp.NewStore(userConfigPath, os.LookupEnv, damcp.Options{})
policy, err := store.Load(ctx)
if err != nil {
    return err
}
resolution, err := policy.Resolve(projectRoot, projectServers, trustProjectForRun)
if err != nil {
    return err
}
```

`Allowed` may connect, `Disabled` must not connect, and `Prompt` still needs an
interactive host decision. Allow-once is session-only whole-project trust. A
remember-subset choice calls `Store.Remember` for only the selected names, while
the current session may still grant all servers shown in the prompt. Denial writes
nothing. After a successful remembered choice or OAuth login, a running host must
reconnect before it can advertise the new tool set; reconnect scheduling and its
viewer/modal are host UI responsibilities.

The store must point to operator-controlled user configuration. Its required
`LookupEnv` must read the process environment, never a project `.env`. Explicit
disabled names override every grant. Approvals bind the canonical project identity,
server name, and canonical JSON fingerprint. Fixed remote URLs may share a validated
Git common-directory identity across linked worktrees; stdio, ambiguous, and
interpolated definitions remain exact-worktree grants. Any definition or transport
change asks again.

Persistence is bounded, cancellation-aware, serialized within a store, atomically
replaced, and mode 0600. Symlinks, special files, oversized input, malformed policy,
and unreadable deny lists fail closed. Errors and diagnostics never include server
definitions, headers, commands, or tokens.

Compared with the pinned Python implementation, numeric JSON tokens retain their
source lexical form while objects are sorted and strings use the same unescaped
Unicode representation. Reformatting a numeric token can therefore cause a safe
extra prompt; it cannot broaden an approval. The Go package also imposes finite
configuration, definition, name, and environment limits and requires the caller to
supply the trusted config path and process-environment lookup explicitly.
