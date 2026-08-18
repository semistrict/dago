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
`--max-retries N`. Its compiled distribution provides OpenAI, OpenRouter, and the
existing caller-owned subscription flow. Selecting another catalog provider produces
an explicit unavailable-factory error until an application compiles and registers that
provider integration; it never downloads an SDK or guesses an OpenAI-compatible wire
protocol.

`--default-model MODEL` stores a normalized explicit spec and exits without model
authentication. `--default-model` shows it, and `--clear-default-model` removes it.
Successful sessions record the recent explicit model in the same owner-private atomic
configuration file. Startup chooses an explicit CLI model, then stored default, then
stored recent, then the useful built-in fallback.

Ollama discovery remains a separate, explicitly invoked operation. Model resolution
never scans packages, calls `/api/tags`, probes endpoints, or performs any other
automatic network discovery.
