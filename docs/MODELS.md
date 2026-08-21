# Model configuration

`daproviders/modelconfig` implements model-name parsing, provider selection,
credential and endpoint resolution, construction-option precedence, runtime-profile
overrides, and default/recent persistence. It deliberately does not import optional
provider SDKs or discover models over the network. Applications register only the
factories they compiled in.

## Resolution

Explicit `provider:model` input always wins. Bare names use exact custom model
declarations first, then the pinned families: GPT/o-series to OpenAI, Command to
Cohere, Mistral/Mixtral to Mistral AI, DeepSeek, Grok, Sonar, Claude, Gemini,
Nemotron/NVIDIA, and Fireworks account paths. Bedrock dotted identifiers, including
regional inference profiles and `:version` suffixes, are recognized before the
ordinary colon split. Claude and Gemini select Vertex AI only when a Vertex project
is present and a Google API key is not.

The built-in catalog includes:

| Provider | Credential environment | Base-URL environments |
|---|---|---|
| `anthropic` | `ANTHROPIC_API_KEY` | `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_URL` |
| `azure_openai` | `AZURE_OPENAI_API_KEY` | `AZURE_OPENAI_ENDPOINT` |
| `baseten` | `BASETEN_API_KEY` | `BASETEN_BASE_URL`, `BASETEN_API_BASE` |
| `bedrock` | ambient AWS authority | — |
| `claude_agent` | ambient Claude CLI authority | — |
| `cohere` | `COHERE_API_KEY` | `CO_API_URL` |
| `deepseek` | `DEEPSEEK_API_KEY` | `DEEPSEEK_API_BASE` |
| `fireworks` | `FIREWORKS_API_KEY` | `FIREWORKS_BASE_URL`, `FIREWORKS_API_BASE` |
| `google_genai` | `GOOGLE_API_KEY` | `GOOGLE_GEMINI_BASE_URL` |
| `google_vertexai` | ambient Google authority/project | — |
| `groq` | `GROQ_API_KEY` | `GROQ_BASE_URL`, `GROQ_API_BASE` |
| `huggingface` | `HUGGINGFACEHUB_API_TOKEN` | `HF_INFERENCE_ENDPOINT` |
| `ibm` | `WATSONX_APIKEY` | `WATSONX_URL` |
| `litellm` | `LITELLM_API_KEY` | — |
| `meta` | `MODEL_API_KEY` | `MODEL_API_BASE` |
| `mistralai` | `MISTRAL_API_KEY` | `MISTRAL_BASE_URL` |
| `nvidia` | `NVIDIA_API_KEY` | `NVIDIA_BASE_URL` |
| `ollama` | optional | caller-selected endpoint |
| `openai` | `OPENAI_API_KEY` | `OPENAI_BASE_URL`, `OPENAI_API_BASE` |
| `openai_oauth` | caller-owned OAuth session | caller-selected endpoint |
| `openrouter` | `OPENROUTER_API_KEY` | `OPENROUTER_API_BASE` |
| `perplexity` | `PPLX_API_KEY` | `PERPLEXITY_BASE_URL` |
| `together` | `TOGETHER_API_KEY` | `TOGETHER_API_BASE` |
| `xai` | `XAI_API_KEY` | `XAI_API_BASE` |

For every environment name, a present `DEEPAGENTS_CODE_` variant wins over the
canonical name, including when it is deliberately empty. Stored credentials win over
environment credentials. A stored key and its paired endpoint are resolved together;
a stored key with no endpoint suppresses an inherited gateway URL. A request endpoint
wins over a provider-configured endpoint, then the stored pair, then environment.

## Factories and precedence

Construction does no I/O until `Resolve` invokes the selected caller factory. Parsing,
provider status, and preference operations never call factories. A minimal setup is:

```go
credentials := dacredential.NewStore(authPath, time.Now, dacredential.Options{})
resolver := modelconfig.NewResolver(credentials, os.LookupEnv, factories, modelconfig.Options{})
resolved, err := resolver.Resolve(ctx, "openrouter:provider/model", modelconfig.ResolveOptions{})
```

The factory receives the normalized `Spec`, credential resolution, and a defensive
`Construction` value. Parameters merge in this order, lowest first: provider profile,
provider configuration, per-model configuration, request `Parameters`, and the
request `MaxRetries`. Runtime-profile overrides merge provider, per-model, then request
values and are applied by the resolver after construction. A zero request retry count
is meaningful and disables adapter retries. The zero resolver options use a bounded
six-retry default for providers with a retry-count constructor parameter.

Custom providers are static `Provider` declarations plus factories. Required
credentials remain positional at the resolver boundary. Invalid declarations panic at
construction; external model input, credentials, endpoints, options, I/O, and factory
failures return errors. Factory errors preserve `errors.Is` identity while known
credential and secret-shaped option values are redacted.

## Coding-agent flags

`dacode` accepts `-M/--model`, `--model-params JSON`, `--profile-override JSON`, and
`--max-retries N`. Its compiled distribution provides OpenAI, OpenRouter, direct
Anthropic Messages API, and the existing caller-owned subscription flow. It also provides `claude_agent`, which runs
the installed Claude CLI in print mode with bidirectional stream JSON. For example,
`--model claude_agent:sonnet` uses the CLI's ambient authentication; `cli_path`,
`context_window`, and `max_output_tokens` are the supported model parameters.

The Claude agent adapter supplies the request system prompt explicitly and starts a
persistent print-mode process in an empty workspace while preserving the user home needed
for the CLI's existing subscription/keychain login. It loads no user/project/local
setting sources, supplies an empty settings object, and disables Claude's built-in tools,
slash commands/skills, browser integration, and prompt suggestions. Strict MCP
configuration publishes only the request's actual
tool schemas through an authenticated ephemeral loopback server. Tool execution remains
in the outer dago loop: its results fulfill the pending MCP calls, allowing ordinary
turns to continue in the same in-memory CLI session. Claude's partial text and reasoning
events are forwarded as native model chunks, so interactive and non-interactive `dacode`
runs render output before the model turn completes. If that process exits or the caller
restores a conversation into a new client, the adapter writes the prior messages in
Claude's native JSONL format under the unique temporary-workspace project entry and
starts the replacement with `--resume`; JSONL is not rebuilt on ordinary calls. Closing
or replacing the process removes that project entry and the temporary workspace.
Admin-managed Claude policy remains an upstream CLI
boundary and cannot be disabled by a child process. `--safe-mode` is not used because it
also disables the explicit MCP server required for caller-owned tools.

The direct `anthropic` provider uses the Messages API with native SSE streaming,
structured output, adaptive reasoning effort, prompt-cache breakpoints, citations,
image/PDF/file blocks, custom and parallel tool calls, and exact replay metadata for
provider-hosted blocks. Hosted web search is enabled by default. `hosted_tools` accepts
the current web fetch, code execution, computer use, memory, tool-search, and MCP
toolset definitions; `mcp_servers` configures remote MCP servers; `betas` adds explicit
beta headers. Remaining `--model-params` are passed as top-level Messages fields, which
keeps context management, containers, task budgets, user profiles, and later additive
API fields available without another adapter release. Required beta headers for known
hosted tools and current task-budget, user-profile, and remote-MCP fields are inferred.

Selecting another catalog provider produces
an explicit unavailable-factory error until an application compiles and registers that
provider integration; it never downloads an SDK or guesses an OpenAI-compatible wire
protocol. OpenAI models enable the Responses API hosted `web_search` tool by default;
set `--model-params '{"web_search":false}'` to disable it. A configured local
`web_search` tool is retained only when the resolved model lacks hosted search.

`--default-model MODEL` stores a normalized explicit spec and exits without model
authentication. `--default-model` shows it, and `--clear-default-model` removes it.
Successful sessions record the recent explicit model in the same owner-private atomic
configuration file. Startup chooses an explicit CLI model, then stored default, then
stored recent, then the useful built-in fallback.

Ollama discovery remains a separate, explicitly invoked operation. Model resolution
never scans packages, calls `/api/tags`, probes endpoints, or performs any other
automatic network discovery.
