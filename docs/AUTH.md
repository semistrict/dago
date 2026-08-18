# Credential store and auth CLI

`dacredential` manages the version-1 provider credential document at
`~/.deepagents/.state/auth.json`. Applications may select a different path by
constructing `Store` with an explicit positional path; `dacode auth` exposes
`--auth-file PATH` for deterministic automation and recovery.

The JSON document contains a `credentials` object keyed by normalized provider
or service name. Records are discriminated by `type`: `api_key` stores the key,
UTC `added_at`, and optional paired `base_url`/LangSmith `project`; `oauth`
stores access/refresh tokens and expiry. Unknown or malformed individual
records fail closed and are counted, while corrupt, oversized, linked, or
unsupported documents fail the whole read. Values and errors implement
secret-free formatting. Writes use an owner-only temporary file, sync it, and
atomically replace the final mode-0600 file below mode-0700 state directories;
Windows uses replace-existing/write-through move semantics.

The pinned registry covers the 20 model-provider environment mappings plus
`openai_oauth`, Tavily, and LangSmith. Resolution checks a valid stored record
first, then `DEEPAGENTS_CODE_<ENV>`, then the canonical environment variable. A
present empty prefixed value shadows the canonical variable, but never a stored
credential. Status and list output contain only `stored`, `env: NAME`, or
`missing` plus a non-secret credential type.

The non-interactive commands are:

```text
dacode auth list
dacode auth status PROVIDER
dacode auth set PROVIDER [--from-env VAR] [--base-url URL] [--project NAME]
dacode auth remove PROVIDER
dacode auth path
```

All commands accept `--json` and `--auth-file PATH`; `--state-dir PATH` selects
the existing OpenAI subscription-session location for list, status, and remove.
`auth set` reads a bounded
key from non-terminal standard input unless `--from-env` names a caller-owned
environment value. It never accepts a key as a positional argument and never
prints one. Omitting optional endpoint/project flags preserves their stored
values; an explicit empty value clears one. LangSmith accepts `us` and `eu`
endpoint aliases, and only LangSmith accepts `--project`.

The coding agent resolves a stored OpenAI API key before its environment key.
The stored key and endpoint are a coherent pair: a stored endpoint overrides an
inherited endpoint, while a stored key without an endpoint clears that inherited
gateway and uses the provider default. Existing OpenAI subscription login keeps
its richer refreshable session in `openai-oauth.json`; `auth set openai_oauth`
therefore rejects API-key input. The generic store's OAuth record remains
available to integrations that own an OAuth refresh flow, and all auth commands
can report or remove such a record without rendering its tokens.

Provider-neutral integrations resolve any registry entry through
`Store.Resolve(ctx, name, lookup)`. The caller-supplied lookup avoids hidden
process-environment access. Resolution is stored key, then a present
`DEEPAGENTS_CODE_<ENV>` value, then the canonical environment name; an explicit
empty prefixed value shadows the canonical value. Tavily and LangSmith are
marked as services and use `TAVILY_API_KEY` and `LANGSMITH_API_KEY`. Values from
the environment receive the same control-character and byte-limit validation as
stored values. Applying a resolved service credential to a network client,
tracing sink, or spawned process remains the caller's explicit responsibility.

## Interactive credential manager

`/auth` and its `/connect` alias open the terminal credential manager. The manager
refreshes through immutable generation-tagged snapshots, so a late status read cannot
overwrite a newer save or removal. API-key entry is masked, bounded, and cleared on
submit, cancel, Ctrl+C, Ctrl+D, or shutdown. Save and removal use the same private
atomic `dacredential.Store`; raw values and provider errors never enter terminal
messages or scrollback.

The OpenAI subscription row starts the existing PKCE flow with a generation-tagged
controller. Esc or Ctrl+C cancels it, late callbacks are ignored, and Ctrl+D cancels
before quitting. Success refreshes the manager without exposing the session document.
Credential changes intentionally affect only subsequently constructed provider
models; the active runner is not silently replaced while work is in progress.
