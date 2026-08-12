# Studio example

This network-free example verifies the local Agent Server and LangSmith Studio
integration with the reusable predictable model.

From the repository root:

```sh
go run ./cmd/dago dev -c examples/studio/dago.json
```

The command prints the local API and hosted Studio URLs and opens Studio. Try
`hello`, `echo: some text`, or `think: a private reasoning note`. Threads,
checkpoints, and store values persist under the ignored
`examples/studio/.dago_api/state` directory.

Application factories use the server-owned saver and store so runs and Studio
operations share durable state. Replace the predictable model in `agent.go` with
any `damodel.Chat` implementation for a live application.
