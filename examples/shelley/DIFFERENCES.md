# Intentional differences from Deep Agents

Deep Agents Python is the behavioral authority for the agent harness used by this
example. shelley-in-dago-specific behavior is retained only at the application boundary:
HTTP and SSE payloads, database projections, OAuth and model discovery, browser UI,
notifications, terminal sessions, and application-only tools.

The following differences are intentional:

- dago requires delta channels. shelley-in-dago does not offer a non-delta execution or
  checkpoint mode.
- SQLite and PostgreSQL use the Python-compatible schemas and language-neutral
  payload subset. Python-specific serialized objects, imports, constructors, and
  callable payloads are rejected instead of being deserialized.
- dago exposes one context-aware Go execution API rather than separate synchronous
  and asynchronous Python APIs. Cancellation and streaming behavior remain part of
  the same contract.
- Shell execution requires an explicitly configured sandbox. A filesystem backend
  alone never grants command execution authority.

Temporary migration gaps are not intentional differences and must not be added to
this file. They remain implementation work until shelley-in-dago delegates the corresponding
harness responsibility to dago.
