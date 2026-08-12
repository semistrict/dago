# Loop package

The `loop` package is shelley-in-dago's product-facing session adapter around dago's
native agent runtime. dago owns model calls, tool execution, durable checkpoint
state, and continuation. shelley-in-dago projects native messages and events into its
database and SSE shapes for the existing HTTP API and UI.

## Runtime contract

- `Config.Model` is a dago `damodel.Chat` implementation.
- `Config.Tools` contains dago `datool.Tool` implementations.
- `Config.Saver` is a dago `checkpoint.Saver`; the server uses the conversation
  ID as `Config.ThreadID`.
- `RecordMessage` and the streaming callbacks maintain shelley-in-dago's UI projection.
- The package does not contain a second model or tool-execution state machine.

## Basic usage

```go
agentLoop := loop.NewLoop(loop.Config{
	Model:         chatModel,
	ModelID:       "gpt-5.6-luna",
	History:       history,
	Tools:         nativeTools,
	Saver:         saver,
	ThreadID:      conversationID,
	System:        systemPrompt,
	RecordMessage: recordMessage,
})

agentLoop.QueueUserMessage(llm.UserStringMessage("Help me inspect this project"))
if err := agentLoop.ProcessOneTurn(ctx); err != nil {
	return err
}
```

The `llm.Message` values at this boundary are shelley-in-dago's persisted and rendered
message projection. Before execution, the loop converts them to native dago
messages; dago checkpoint state remains canonical across turns.

## Deterministic tests

`NewPredictableService` returns a native `damodel.Chat` implementation with fixed
responses and tool calls used by shelley-in-dago's Go and browser suites:

```go
chatModel := loop.NewPredictableService()
agentLoop := loop.NewLoop(loop.Config{
	Model:         chatModel,
	RecordMessage: recordMessage,
})
```

Tests can inspect `GetLastRequest` when they need to assert the exact dago
request sent by the loop.
