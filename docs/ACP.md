# Agent Client Protocol

`daacp` implements ACP v1 and exposes versioned extensions for model discovery
and background dago workflows. Workflow methods are available when the session
runner implements `daacp.WorkflowSource`; other runners retain ordinary ACP
behavior.

## Model discovery extension

Clients can inspect the model selector without creating a session by sending
`_dago/models/list`:

```json
{ "version": 1 }
```

The response is derived from the same model configuration returned by
`session/new` and `session/load`:

```json
{
  "version": 1,
  "default_model": "gpt-5.6-terra",
  "models": [
    { "id": "openai:gpt-5.6-terra", "name": "GPT-5.6 Terra" }
  ]
}
```

Clients must treat model identifiers as opaque values and return the selected
identifier through the normal ACP session configuration methods.

## Workflow lifecycle extension

Every payload currently uses `version: 1`. Clients must reject versions they do
not understand rather than guessing at their shape.

The agent publishes `_dago/workflow/update` notifications with this envelope:

```json
{
  "version": 1,
  "session_id": "session-id",
  "workflow": {
    "version": 1,
    "task_id": "workflow-1",
    "run_id": "wf_1",
    "name": "review",
    "status": "running",
    "created_at": "2026-08-22T12:00:00Z",
    "updated_at": "2026-08-22T12:00:01Z"
  }
}
```

`workflow` is a complete `daworkflow.Status` snapshot, not a patch. It includes
declared phases, the ordered event journal, token-bearing agent events, terminal
result or error data, and persisted script/transcript/output paths when configured.
Delivery is non-blocking and latest-state: a slow client may miss intermediate
snapshots, so it must derive current UI state from each full snapshot.

Clients can resynchronize by sending `_dago/workflow/list`:

```json
{ "version": 1, "session_id": "session-id" }
```

The response is `{ "version": 1, "workflows": [...] }`, where each item is the
same complete workflow status used by update notifications.

To stop one active run, clients send `_dago/workflow/cancel`:

```json
{ "version": 1, "session_id": "session-id", "run_id": "wf_1" }
```

The request returns `{ "version": 1, "run_id": "wf_1", "status": "cancelling" }`.
The terminal `cancelled` workflow snapshot arrives through the normal update
notification. Invalid sessions, missing runs, settled runs, and malformed payloads
return ACP invalid-params errors. Unknown extension methods return method-not-found.

ACP `session/cancel` still cancels the active model turn. A host that presents one
combined Stop action should cancel its running workflows with the extension before
issuing `session/cancel`.
