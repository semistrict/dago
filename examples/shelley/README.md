# Shelley example

This is a complete, mobile-first coding-agent application built on Dago. It is
an independently written example inspired by the interaction surface of the pinned
Apache-2.0 Shelley reference listed in `docs/upstream-manifest.json`.

It includes:

- durable multi-conversation history and forking through the SQLite checkpointer;
- streamed model tokens, graph events, tool calls, usage, cancellation, and human
  approval for shell execution and recursive deletion;
- API-key and subscription OAuth access to OpenAI models;
- a confined local workspace with explicit shell execution, or connection to an
  existing LangSmith sandbox through `langsmith-go`;
- attachments and multimodal messages, file search/editor/download, Git status and
  diff inspection, a terminal, command palette, themes, export, and responsive UI.

Run it from the repository root:

```sh
go run ./examples/shelley -workspace /path/to/project
```

Then open `http://127.0.0.1:9000`. Configure a credential and model in Settings.
The default data directory is the operating system’s user configuration directory;
override it with `-data`.

The local backend can execute host processes inside the selected workspace. This
example is single-user and has no HTTP authentication. Keep it bound to loopback
unless an external access-control layer is added. The LangSmith option only connects
to an existing sandbox; it never creates or deletes remote resources.
