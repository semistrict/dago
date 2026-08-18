# Compatibility matrix

Status values are `verified`, `implemented`, `deferred`, and `intentionally different`.

| Surface | Status | Evidence or boundary |
|---|---|---|
| Explicit model and custom tool construction | verified | `dagent/agent_test.go`, `dago_test.go` |
| Typed state fields, reads, dependencies, metadata, resume, and structured output | verified | typed adapter, checkpoint-shaped decode, approval-resume, metadata, and schema-validation tests |
| Middleware order and wrapper nesting | verified | `dagent/agent_test.go` |
| Hooks v2 lifecycle and hooks.json | verified | `dahook` covers all twelve pinned client/server lifecycle events; project/user/enabled-plugin layering and explicit workspace trust; matcher-aware concurrent execution with precedence-ordered reduction; direct argv and shell handlers; compatible legacy-list migration with original working-directory preservation; bounded process-tree cancellation/output on Unix and Windows; capability-authenticated versioned server interrupt fulfillment with atomic client/server replay rejection; and private versioned per-thread/subagent transcript projections in deterministic offline tests |
| Parallel tool calls and deterministic reduction | verified | agent and graph tests |
| Provider and synthetic-tool structured output | verified | schema validation and retry tests |
| Mandatory delta state and message channels | verified | channel, graph, memory saver, SQLite, remove-all reset, and pre-checkpoint stable-message-ID tests |
| Graph routing, sends, retry, interrupt, resume, and parent handoff preservation | verified | graph tests plus agent after-hook model re-entry, invalid-destination guards, and typed terminal handoff tests |
| Replay by checkpoint and thread fork | verified | public history/replay/fork helpers and saver copy/replay tests |
| Namespaced durable store search | verified | the pinned finite ten-item search default, explicit limit/offset validation, bounded SQLite row materialization, and paginated complete backend snapshots have memory and SQLite tests |
| SQLite standard schema and saver behavior | verified | schema, conflict, finite-default history, negative-limit rejection, explicit default/custom codec construction, nil-database rejection, copy, prune, and restart tests |
| PostgreSQL migrations 0–9 and saver behavior | verified | immutable migration inspection, finite-default history, negative-limit rejection, explicit default/custom codec construction, nil-database rejection, migration tests, and live saver integration gate |
| Safe checkpoint payload subset | verified | shared public codec contract, serde round trips, portable named-scalar normalization, approval-interrupt persistence, and rejection tests |
| Bidirectional Python payload fixtures | verified | SQLite and PostgreSQL are generated, read, and continued in both directions |
| State, memory, store, filesystem, composite, local shell | verified | shared backend and thread-scoped delta-state tests; interactive local mode matches Dcode's real-path contract by exposing the actual working directory to file and shell tools, resolving relative shell paths from that directory, and treating `/` as the host root, while remote sandboxes retain provider paths |
| Reusable sandbox backend derivation | verified | `SandboxBackendProtocol` + `BaseSandbox` derive listing, paginated/bounded reads, writes, server-side edits, delete, glob, literal grep, upload, and download from execute + upload in Python-equipped images; opt-in capture-at-source preserves exit status, caps saved output, and falls back safely |
| Persistent JavaScript interpreter and programmatic tool calling | verified | normal Go builds use the pinned quickjs-rs WASM guest under Wazero's portable interpreter; TinyGo builds reject explicit enablement and Shelley omits `js_eval`; full applicable upstream suite map in `docs/QUICKJS_TEST_PORT.md`; Promise settlement, typed PTC, interruption, WAFL dirty-page checkpoints, memory/SQLite/PostgreSQL restore, subagent dispatch, and browser execution tests |
| Rooted local filesystem confinement | verified | `os.Root` operations plus traversal, symlink-escape, write, delete, glob, grep, upload, and download tests |
| Filesystem result safety, media compaction, partial/uncapped grep, and transfer batching | verified | root filesystem contracts plus backend concurrency, pagination, deterministic grep, partial-error, and composite batch tests |
| Managed helper tools | intentionally different | grep is a native bounded backend operation rather than a downloaded `ripgrep` executable; `/tools` lists the effective agent toolset in both native and browser terminals, so there is no helper binary to install or repair |
| LangSmith remote sandbox | verified | SDK adapter tests; live test is credential-gated |
| Runloop remote sandbox | verified | pinned `libs/partners/runloop` behavior via a caller-supplied transport; stable devbox identity; 30-minute command default; bounded native upload/download; derived filesystem operations; attach/create/delete and paginated blueprint lifecycle tests use no network |
| Daytona remote sandbox | verified | pinned `libs/partners/daytona` behavior via a caller-supplied transport; stable sandbox identity; session-log polling with fixed/adaptive intervals; explicit-zero indefinite waits; exit-124 timeouts; stderr tagging; bounded native batch transfers; and cleanup/cancellation tests use no network |
| Modal remote sandbox | verified | pinned `libs/partners/modal` behavior via a caller-supplied transport; stable sandbox identity; direct `bash -c` argv boundary; 30-minute command default with explicit-zero support; absolute-path validation; bounded native upload/download and derived filesystem operations; tests use no network |
| Vercel remote sandbox | verified | pinned `libs/partners/vercel` behavior via a caller-supplied transport; detached `bash -lc` execution, 30-minute default and explicit-zero wait, exit-124 timeout with bounded cleanup, bounded/tagged output with log-fetch fallback, batch upload error projection, native download error classification, and derived filesystem operations; tests use no network |
| AgentCore Code Interpreter remote sandbox | verified | pinned `deepagents_code.integrations.sandbox_factory.AgentCoreSandboxProvider` and delegated adapter behavior via a caller-supplied transport; stable session identity; lazy working-directory discovery; streamed result/error projection; text/blob native transfers; AWS region precedence; fresh-session create/delete lifecycle; no reconnect; and finite command, output, event, file, batch, start, stop, and active-session limits have network-free adversarial tests |
| Remote sandbox registry and command routing | verified | `dasandbox` publishes deterministic metadata for the six curated providers, explicit Go factory registration with configuration-over-extension-over-built-in precedence, attach and snapshot capability validation, provider working directories, bounded literal setup upload, fresh-resource cleanup, attached-resource retention, and panic/error redaction; dacode requires explicit `--sandbox` selection and supports ID, snapshot/blueprint, setup-file, configured-default, ordinary, and ACP session routing without credential discovery |
| Docker local sandbox | implemented | hardened creation, workspace confinement, execution, cancellation restart, cleanup, and opt-in live-container tests |
| Context Hub persistent agent repository | verified | lazy pull, commit chaining, linked entries, cache recovery, batch transfer, and LangSmith SDK transport tests |
| Provider and sandbox construction contracts | verified | OpenAI/OpenRouter credentials and model IDs, OAuth URL delivery, remote transports, sandbox identities, stores, and factories are positional dependencies; network-free construction panics on static mistakes and rejects typed nils, while Docker, browser restoration, OAuth, and explicit remote lifecycle boundaries retain errors for I/O; omitted limits, clients, endpoints, retry schedules, and browser bridge names select documented bounded defaults |
| Filesystem tools and permission approval | verified | root vertical-slice tests |
| Declarative and precompiled subagents, isolation, state propagation, and nested approval resume | verified | root and agent tests |
| Custom filesystem subagents | verified | `dasubagent` deterministically discovers bounded `NAME/AGENTS.md` definitions from the selected user profile and project, applies project-over-user precedence, supports optional model inheritance or override, and rejects links, special files, replacement races, invalid YAML, oversized input, and unsafe names; dacode replaces the built-in `general-purpose` agent only when that exact custom definition exists and otherwise retains it |
| Inline subagent todo isolation and operational failure propagation | verified | root subagent isolation, recoverable-argument, and child failure tests |
| Invocation-scoped runtime context | verified | graph runtime, concurrent invocation, inline subagent, and rubric grader propagation tests |
| Layered configuration and runtime model context | verified | `daconfig` compiles a canonical typed manifest, resolves bounded defaults/files/canonical env/`DEEPAGENTS_CODE_`/`DEEPAGENTS_CLI_`/explicit overrides, atomically manages its versioned file, redacts credentials, and round-trips a typed non-credential `ServerConfig` environment payload; `dacode config` exposes deterministic show/get/path/set/unset text and JSON commands, while `dagent.RuntimeModel` provides validated concurrent per-run selection with private checkpoint fallback, cancellation, panic containment, and a required caller resolver |
| Credential store and auth CLI | verified | `dacredential` provides the versioned, bounded, owner-private atomic `auth.json` store, discriminated API-key/OAuth records, secret-free formatting, stored-over-prefixed-over-canonical resolution, and the pinned 20-provider plus OpenAI subscription, Tavily, and LangSmith registry; `dacode auth list|set|remove|status|path` reads keys only from bounded stdin or an explicitly named environment variable, reports no secret values, manages existing subscription state, and has deterministic filesystem, cancellation, concurrency, service, and command-dispatch tests |
| Multi-provider model configuration and resolution | verified | `daproviders/modelconfig` provides the pinned 23-entry provider catalog (20 API-key providers plus Bedrock, Ollama, and subscription OAuth), exact/custom/bare/Bedrock detection, stored and prefixed/canonical credential status, paired and overridden base URLs, provider/model/request parameter and profile precedence, bounded retry overrides, caller-owned factories, and owner-private default/recent persistence; dacode exposes the pinned flags and compiled OpenAI/OpenRouter/subscription factories without automatic network discovery |
| Managed-agent sandbox backend configuration | verified | `damanaged` normalizes legacy state and scoped-sandbox aliases to the pinned payload, merges canonical sandbox configuration over the legacy field, defaults scope, preserves exact JSON integers, rejects boolean/fractional TTLs and non-string policy IDs, and emits a migration error for removed `runtime.backend_type` |
| Layered dotenv loading | verified | `daenv.Load` resolves shell, nearest-project, and user-global layers with deterministic first-write precedence; dacode applies and restores only newly injected values; dangerous process-launch keys are denied from every file and project-controlled proxy, TLS, tracing, MCP-trust, automatic-review, and terminal keys are denied from untrusted project files; file/link/replacement/ancestor/line/key/value/total limits have offline adversarial tests |
| First-run onboarding launch policy | partial | The owner-private completion marker and canonical `startup.onboarding` file/environment policy feed one deterministic app-neutral launch decision; explicit launch requests take precedence, unset policy follows the marker, malformed environment values fall back safely, and non-interactive execution cannot open the flow. TUI lifecycle wiring and browser coverage remain pending. |
| External event bus Unix ingress | verified | `daeventbus` exposes the pinned command, prompt, signal, bypass, correlation, and ACK/NACK vocabulary through a required caller sink and absolute socket path; private-directory and socket modes, stale/active/replacement handling, finite line/payload/client/event/time limits, cancellation, panic containment, deterministic replies, Unix behavior, and the explicit Windows unsupported result have adversarial tests |
| Ollama local model discovery | verified | `daproviders/ollama` performs an explicit caller-triggered `/api/tags` request through a required caller transport, accepts only normalized literal-loopback origins, applies finite time/payload/count/name bounds, returns deterministic unique names, propagates cancellation, contains transport panics, and has network-free adversarial tests |
| LangSmith LLM Gateway routing | verified | `daproviders/langsmithgateway` resolves the pinned Anthropic, Baseten, Fireworks, Google GenAI, and OpenAI `provider:model` routes through a required caller factory and positional gateway credential; construction and inspection are network-free, provider sets and model input are bounded, cancellation is preserved, and factory errors, panics, and typed nils have deterministic adversarial tests |
| Durable rubric terminal outcomes and repository-backed self-evaluation | verified | public result and persisted-checkpoint status tests; bounded read-only repository evidence; dynamic grader settings; sticky, file-backed, and restoring one-turn TUI controls; one transient grader-transport replay |
| Durable thread goals, server-drafted acceptance criteria, constrained model tools, host lifecycle controls, usage accounting, continuation messages, and criteria-gated completion | verified | `dagoal` drafting, fallback, persistence, tool, service, staged-completion, review-widget, persistent-panel, and terminal-browser tests |
| Interactive mid-run questions | verified | `daaskuser` is enabled in dacode; tagged portable interrupts drive required/optional text and multiple-choice prompts with Other, keyboard question navigation, multiline and external-editor answers, dismissal, durable portable resume, terminal-safe agent text, collapsed answer/failure summaries, and answer-free automatic-review audit input; Go, SQLite, and browser-terminal tests cover the flow |
| Task-scoped structured output for declarative subagents | verified | per-task schema validation and precompiled-runnable rejection tests |
| Summarization, offload, and compaction | verified | AND/OR/fraction threshold clauses, valid cutoffs, thread-aware offload, state-update tests, and TUI `/offload`/`/compact` execution that seeds the tool server-side and atomically commits only successful exact-checkpoint updates |
| Skills subsystem, CLI, built-ins, and memory prompt injection | verified | Safe YAML and deterministic progressive disclosure; pinned low-to-high built-in/plugin/profile/user/project precedence with plugin-qualified names; bounded duplicate resolution; exact canonical external-symlink trust with repointing protection; `dacode skills list|info|create|delete|trust|inspect-thread`; TUI `/skills`, `/skill:NAME`, `/remember`, and `/skill-creator`; built-in remember, skill-creator, and read-only thread-inspector instructions; intended/adversarial Go tests and terminal Playwright coverage. |
| Persistent memory managed blocks | verified | `Memory.ReadOnly` selects a useful non-writing default prompt; configured memory sources automatically guard machine-owned marker blocks for the main agent and shared-backend subagents; dacode mounts one shell-inaccessible `AGENTS.md` per selected/default agent, pins resumed threads to their original agent, and loads user memory before project guidance without duplicating it; malformed and duplicate markers fail closed; unrelated edits, CRLF files, symlink aliases, concurrent calls, routed confinement, and durable restoration have intended-behavior tests. |
| Workspace instruction discovery | verified | `daworkspace` precedence, trust gating, deduplication, scoped-file, directory-filter, and JSON-contract tests plus shelley-in-dago prompt coverage |
| Local environment context injection | verified | opt-in `daworkspace.LocalContext` middleware takes one cache-stable snapshot per checkpointed session by running a framework-owned, 30-second-bounded detection script only through an explicitly supplied sandbox; stable project, package-manager, runtime, Git, test-command, file/tree, Makefile-target, and optional GitHub CLI sections; private state; untrusted-data prompt boundary; graceful discovery failures; cancellation propagation; and coding-agent wiring tests |
| Git repository metadata | verified | `dagit` filesystem-first branch, full commit SHA, and origin URL reads; loose and packed refs; linked-worktree common metadata; reciprocal trusted common-directory validation; bounded context-aware subprocess fallback; and normalized provider/repository parsing tests |
| Built-in web tools and SSRF boundary | verified | opt-in `daweb` HTTP request, HTML-to-Markdown page fetch, and Tavily search tools; public-address validation, DNS pinning, per-hop redirect checks, proxy bypass, cross-origin credential/body containment, cancellation, and finite request/response/render/redirect/time limits; dacode always binds `fetch_url` and binds approval-gated `web_search` only at process startup when its stored-over-environment Tavily service credential resolves; adversarial tests use no live network |
| Provider model-spec resolution and harness profiles | verified | explicit caller-owned factories; layered OpenAI/NVIDIA/OpenRouter construction profiles; alias-aware matching; Bedrock detection; built-in Anthropic harness overlays; thread-safe additive provider and harness plugin registration; isolated plugin loading failures; explicit Nemotron profile composition; active repair, retry, budget, policy, entity, follow-up, and final-answer contracts |
| GLM-5.2 harness profile and terminal-stall recovery | verified | exact Fireworks, OpenRouter, and Baseten model profiles receive the pinned execution-focused prompt; `GLM52TerminalStallRecovery` is an explicit headless-only middleware that retries only the measured Fireworks max-token/no-tool response, disables reasoning, requires a tool, preserves caller settings, honors cancellation, and cannot retry more than once |
| Installed module version run metadata | verified | Go build-info main/dependency/replacement detection plus `lc_versions.dago` invocation metadata tests |
| Startup housekeeping, dependency floors, and debug tracing | verified | `dahousekeeping` provides idempotent no-clobber state migration with atomic SQLite-sidecar handling, immutable Go build-graph floor diagnostics, bounded redacted resumable traces, secure file opening, and exact `DEEPAGENTS_CODE_DEBUG*` resolution; `WithLogger` routes graph events through an application-owned logger, while `make module-floor` verifies checksums and rejects module-manifest drift |
| Shell command allow-list enforcement | verified | fail-closed parser, curated read-only preset, unspoofable unrestricted mode, every-segment executable checks, dangerous-pattern rejection, inline tool errors, CLI wiring, and bypass tests in `internal/dacode` |
| Offline doctor diagnostics | verified | reusable `dadoctor` API and `dacode doctor`; stable text/JSON sections, truthful manual-signed-update status without network access, build/SDK/platform/install facts, credential-presence and endpoint redaction, bounded Git fallback, path health, cancellation, exit status, and cross-platform tests |
| Signed self-update lifecycle | verified | reusable `daupdate` check, dry-run, and explicitly authorized apply operations plus `dacode update CHANNEL ARTIFACT`; exact Ed25519-signed bounded manifests, signed SHA-256 and byte length, exact Go package/module/version build provenance, stable/prerelease SemVer ordering, HTTPS same-origin retrieval without redirects or ambient proxy use, cross-process mutation locks, target identity checks, atomic same-filesystem activation, cancellation, deterministic no-live-network tests, and Windows fail-closed running-executable behavior |
| Terminal notifications, trace links, and signed updates | verified | `/notifications` and Ctrl+N expose bounded persistent warning controls and stable actionable entries; toasts are finite, terminal-safe, ASCII-aware, and anchored above the composer; `/trace` performs a finite official-endpoint LangSmith project lookup and defers transcript output during streaming; `/update` uses only a complete launch-time signed-update profile with two-step activation; `/auto-update` persists consent, skips, reminders, cooldowns, and restart-loop state with generation-safe visible updates |
| Closed optional-extra and package installer | verified | reusable `dainstall` API and `dacode install NAME`; immutable application-curated catalogs, exact direct argument vectors, explicit mutation authorization, finite execution and lock waits, cross-process serialization, terminal-safe errors, credential-minimized child environments, and process-group cancellation; dacode truthfully identifies included Go integration packages, requires an application import and rebuild, and rejects all arbitrary packages because the binary has no dynamic package format |
| Plugin marketplaces and lifecycle | verified | `daplugin` parses pinned marketplace and plugin manifests, supports local, GitHub, HTTPS Git, and direct HTTPS catalog sources through explicit bounded materialization, atomically tracks marketplaces/installations/enablement, validates identity and canonical realpath confinement, inventories skills/hooks/MCP declarations while surfacing unsupported agents/commands, and composes immutable component sources. `dacode plugin|plugins` dispatches the complete non-TUI lifecycle and marketplace command set before model startup; TUI `/plugins` exposes Discover, Installed, and Marketplaces management with pending-reload state. Each normal run and ACP session discovers enabled installed plugins without network access, mounts namespaced read-only skills, scopes and resolves MCP declarations with plugin environment/data/cwd handling, and executes plugin hooks at the SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, PostToolUseFailure, Stop, and SessionEnd boundaries. `/reload` atomically rebuilds those runtime components and refreshes reloadable dotenv credentials, web tools, shell policy, and MCP configuration while preserving the prior runtime and environment on failure. PermissionRequest, Notification, PreCompact, SubagentStart, and SubagentStop remain supported by `dahook` but dacode does not emit them until it has a faithful host call site; prompt suppression, permission bypass, and stop-loop continuation outputs likewise remain protocol-only. Agents and commands remain explicitly unsupported components. |
| Session token and estimated-cost accounting | verified | `dacost` provides a bounded request ledger with incremental chunk revision, replay finalization, late provider/model re-filing, negative correction handling, cache read/write normalization, exact provider/model and Assistant/Subagent/Offload/Auto buckets, literal-zero price coverage, primary/local/bundled catalog precedence, bounded local override parsing, atomic repricing, and deterministic versioned reports. All runner-resolved models normalize streaming usage while preserving optional capabilities; root checkpoints retain delegated, summarization, rubric, criteria-drafting, and automatic-review ownership transfers; and the runner reconstructs reports with its private `prices.json` plus bundled stopgaps. The terminal status, `/cost`, `/tokens`, and exit summary are generation-safe and bounded, with Go, race, and browser-terminal coverage. |
| Headless coding-agent execution | verified | explicit and automatically detected standard input, streamed/quiet/buffered/versioned-JSON output, durable resume, fail-closed shell exposure, automatic review, goal continuation, cancellation, 50-turn safety default, caller-selected turn and wall-clock bounds, and status-124 integration tests in `internal/dacode` |
| Session bootstrap automation | verified | `-m` initial submissions, `-s` project-skill invocation with priority and confinement, 60-second-bounded `--startup-cmd` execution before skill discovery and the first model turn, and positive `--recursion-limit` parsing with a useful 2000-step default; local setup output is terminal-safe, retained in interactive transcripts, excluded from model context, and routed away from clean one-shot stdout; non-zero exits warn and continue, and cancellation stops the process group |
| Local development subprocess boundary | verified | interactive and one-shot dacode runs can opt into an explicit absolute server executable with literal repeated argv, loopback-only readiness, typed credential-free `ServerConfig`, an exact non-secret environment allowlist, bounded startup/shutdown/logging, cancellation cleanup, Unix process groups, Windows tree termination, concurrent idempotent close, and a serialized restart controller that cannot resurrect after terminal shutdown; intended, race, fake-process, and cross-platform compile tests require no live network |
| Headless MCP write guard | verified | protocol-annotation provenance, coherent read-only classification, fail-closed execution wrapping without an approval policy, exact-name approval rules, and adversarial tests in `internal/dacode` |
| Project MCP trust and approval persistence | verified | `damcp` filters project definitions before transport creation through explicit process grants, reject-precedence denies, per-project definition fingerprints, and a host-owned unresolved prompt set; remembered grants use validated Git-common identity only for fixed remote URLs while stdio/interpolated/ambiguous definitions remain worktree-exact; bounded owner-private atomic TOML persistence, legacy diagnostics, unreadable-policy failure, cancellation, mutation isolation, and offline adversarial tests cover the UI-neutral contract |
| MCP config discovery and standalone client support | verified | standard user, project-subdirectory, and project-root `.mcp.json` layers merge by server name with an explicit-path override; raw project definitions pass through reject-precedence trust before bounded `${VAR}` interpolation; stdio, HTTP, and SSE sessions, per-server tool filters and prefixes, protocol safety annotations, mixed-server failure isolation, redirect refusal, cancellation, and finite config/server/schema/result limits have intended offline and loopback tests |
| Configured MCP OAuth login | verified | `dacode mcp login <server>` selects only discovered user, explicitly trusted, or definition-approved project entries before interpolation; Slack exact-host workspace selection, GitHub Copilot device authorization, and generic MCP PKCE/dynamic registration share bounded paste-back/device interaction and endpoint-bound private atomic token storage. Normal HTTP and SSE startup reuses stored tokens non-interactively, redirects stay disabled, cleartext OAuth and authorization-header combinations fail closed, and parser/trust/provider/token-mode/retry/no-secret tests run without provider network access. |
| Interactive credential and subscription management | verified | `/auth` and `/connect` provide generation-safe status refresh, masked bounded API-key save/removal, cancellable subscription sign-in, late-callback rejection, fixed secret-free errors, private atomic persistence, and explicit current-runtime isolation; intended Go, race, and offline browser-terminal tests cover success, failure, cancel, removal, and alias paths |
| Interactive MCP management | verified | `/mcp`, `/mcp login NAME`, and `/mcp reconnect` provide bounded server/tool/error views, endpoint-safe OAuth interactions including optional empty Slack workspace selection, serialized session enable/disable mutation, reconnect prompts, pending-state retention, late-event rejection, and offline browser-terminal success/error/cancel coverage |
| Batch Auto classifier and model management | verified | Auto inherits the main model unless a separate `provider:model` reviewer is explicitly configured. `/auto model` supports validated session selection, Ctrl+S default persistence, clear-to-main behavior, and generation-safe stale-result rejection. Auto sends one bounded required-tool batch with exact tool-call ID coverage, minimal redacted user/action context, classifier-identity-bound denial/unavailable/config-fault state, pinned 3 consecutive-denial, 2 unavailable, and 20 total-denial fallbacks, cancellation preservation, successful-tool reset, and intended Go/race/browser tests |
| Per-thread approval-mode persistence | verified | raw thread IDs are SHA-256-keyed in private durable state; missing, invalid, corrupt, oversized, and non-regular records fail closed to Manual; live mode changes are acknowledged before the active thread changes; resume restores the selected thread; server-side approval rules consult the record for main and delegated-subagent tools; failed Manual persistence cancels active work and blocks new runs; Go, race, and browser-terminal restart/resume tests cover the contract |
| Interactive approval decisions and HITL widget | verified | warning-bordered single/batch menus have cursor navigation, Enter, semantic and numeric app-level quick keys, thread-Auto/manual-fallback transitions, bounded expandable shell commands, a tool-renderer registry with write content, edit diff, delete path, task-minimal, and deterministic generic previews, plus credential-path content suppression in both the widget and pending tool row; Tab opens a bounded single-line rejection-reason field, with blank and reasoned rejection, cancel-first Esc/`n`, exact model-facing framing, and terminal-safe audit rendering; approvals arriving during recent composer input or Auto-review fallback use a generation-safe two-second idle deferral with a 30-second ceiling, preserving the draft and preventing accidental decisions; submitted drafts remain FIFO behind the approval, Auto review is cancellable without resuming a tool, live Manual/Auto/YOLO transitions reconcile the pending request, and Go plus browser-terminal tests cover each visible path |
| Interactive named-agent selector | implemented | `/agents` discovers fail-closed `AGENTS.md` profiles in the configured state directory; modal keyboard navigation, current/default markers, atomic default persistence, instruction switching, fresh-thread isolation, errors, cancellation, and browser-terminal behavior have intended-behavior tests in `internal/dacode` |
| Resume trust flow | verified | Exact-thread metadata inspection exposes the owning agent, private original directory, latest context size, preview, checkpoint identity, and checkpoint time without loading the thread. The TUI orders cross-agent, directory, then compact-on-resume decisions; pins the approved checkpoint against concurrent replacement; transactionally applies or rolls back agent/directory changes; rejects unavailable agents, malformed paths, mismatched metadata, invalid actions, cancellation, duplicate loads, and pre-decision loads; and uses the pinned 400,000-token default from `threads.compact_on_resume_threshold` (zero disables). Go and browser-terminal tests cover every visible decision path. |
| Interactive lifecycle surfaces | verified | First-run onboarding persists owner-private completion and preferences, `/restart` confirms and restarts only an explicitly owned local server, and bounded progress/error views remain terminal-safe in Unicode and ASCII modes; Go and browser-terminal tests cover completion, persistence, unavailable/error paths, and relaunch behavior. |
| Named-agent profiles and management CLI | verified | `-a/--agent` selects or creates a bounded profile with canonical `agents.default`, then valid `agents.recent`, then built-in fallback; `dacode agents list|ls` and `agents reset --agent NAME [--target SOURCE] [--dry-run]` provide deterministic text/JSON management without model authentication or network access. Profiles isolate bounded `AGENTS.md`, read-only runtime skill discovery, and selected-agent session browsing; reserved/hidden/link-backed state fails closed. |
| External editor composition | verified | `/editor` and Ctrl+X round-trip the current draft through `$VISUAL`, `$EDITOR`, or a useful platform fallback using a private bounded Markdown file and direct argument execution; known GUI and Vim-family compatibility flags, exact newline normalization, blank/non-zero cancellation, terminal restoration, safe display names, confinement checks, and browser-terminal behavior have intended-behavior tests in `internal/dacode` |
| Interactive composer parity | implemented | `!`/`!!` local shell modes, `/` and `@` completion, bounded large-paste placeholders, atomic legacy paste handling, pasted file mentions and bounded structured image/video attachments, private persistent history, clickable clear/copy actions, FIFO busy-turn queueing, modifier-newline and word-deletion compatibility, refocus-click suppression, and selection clipboard fallback are covered by Go and browser-terminal tests |
| Reasoning-effort selection | verified | `/effort`, `/effort LEVEL`, and `/effort clear` use the active model profile's ordered levels and default; the keyboard picker marks current/default values, selected levels affect subsequent model requests, persist per model, and appear in the status bar; unsupported models and levels fail with useful local explanations, with intended-behavior and browser-terminal tests in `internal/dacode` |
| Terminal context-usage visualization | implemented | `/context` renders a color-coded used/free context window with exact provider-reported usage, percentage, model identity, useful unavailable/empty states, Esc dismissal, and browser-terminal coverage |
| Terminal theme system and selector | verified | `/theme` provides live preview with cancel restoration, label/key display, global and `TERM_PROGRAM`-specific atomic preferences, environment precedence, LangChain dark/light, the pinned Textual catalog, ANSI terminal-palette modes, and bounded custom `[themes.NAME]` color overrides; intended Go, race, and browser-terminal tests cover persistence and visible behavior |
| Terminal-safe external text | verified | dacode converts raw C0/C1 controls and deceptive Unicode in transcript, approval, status, model, path, and identifier data to visible markers; browser tests prove raw assistant/tool output cannot invoke clipboard or application URL controls, while supported Markdown links deliberately render as OSC-8 hyperlinks |
| Interactive transcript rendering | verified | Assistant chunks are incrementally rendered as CommonMark/GFM terminal output through Glamour, including working OSC-8 links; user messages, tool output, task arguments, and skill invocations have finite previews and Ctrl+O expansion; consecutive successful/running tools collapse into deterministic lifecycle summaries while errors and rejections remain visible; successful write/edit calls show bounded inline diffs with a persistent `/line-numbers` preference; restored history keeps a 160-item sliding render window and hydrates older retained items on PageUp. Intended Go and PTY-browser tests cover each visible behavior. |
| Token/update/task/interrupt/custom streaming | verified | graph, agent, and provider stream tests, including immediate terminal results from independently completed parallel tools |
| Agent-event and model-chunk iterators | verified | completion, terminal-error, and early-break closure tests while retaining explicit `Next`/`Close` |
| API-key and subscription OAuth model access | verified | request, PKCE, refresh, persistence, HTTP streaming with structured results, default Responses WebSocket transport, incremental continuation, remote V2 compaction trigger/state replay, and cancellation tests |
| OpenRouter Responses model access | verified | API-key authentication, app attribution, provider routing, tool requests, usage, streaming keepalives, typed errors, and an opt-in live structured/tool/stream suite |
| GitHub Actions coding-agent execution | verified | repository-root composite action; explicit task and credential inputs; bounded secure defaults; branch-scoped durable thread caching that excludes credentials; validated GitHub skills checkout; injection-safe command and output handling; cancellation and failure propagation tests in `internal/githubactiontest` |
| Managed-agent project scaffold | verified | `dago init [name]` creates the pinned agent configuration, prompt, empty tool manifest, ignored environment file, example skill, and researcher subagent; omitted names prompt interactively, overwrite requires `--force`, unrelated files remain intact, and path/link adversarial tests cover the local boundary |
| Managed CLI interactive redirect | verified | bare `dago` exits non-zero with an explicit `dacode` migration notice; operational commands remain explicit subcommands |
| Managed-agent deployment | verified | `dago deploy` strictly and finitely loads `agent.json`, `AGENTS.md`, tools, skills, subagents, and extra text; supports credential-free dry-run, serialized and recovery-keyed create-or-metadata-patch upsert, durable external endpoint/project state, explicit declared-target confirmation, reset/detach, complete initial/recovered managed-directory synchronization, parent commits, one conflict refresh, and health reporting with deterministic fake-transport tests |
| Managed-agent list/get/delete CLI | verified | `dago agents list|get|delete` uses a required caller-authenticated bounded `damanaged` client; exact hosted paths, pagination, include-files projection, explicit deletion confirmation, environment precedence, HTTPS-only endpoints, redirect refusal, one bounded server-error retry, cancellation, response/error limits, and network-free transport tests preserve the pinned operator contract |
| Managed MCP server registry and tool discovery | verified | `dago mcp-servers` implements add/list/get/update/delete and paste-ready tool discovery through the bounded caller-authenticated client; exact ID/name/URL resolution, header parsing and redaction, OAuth record mode, explicit destructive confirmation, duplicate/ambiguous rejection, provider-controlled output sanitization, and network-free transport/CLI tests cover the contract |
| Managed MCP server OAuth connection | verified | `dago mcp-servers connect` registers the per-user provider, starts reuse/fresh scoped authorization, validates and prints the HTTPS verification URL, optionally launches the platform browser without a shell, supports start-only mode, and performs cancellation-aware finite long polling with exact status handling and fake-transport tests |
| Generated maintainer wiki | verified | OpenWiki 0.1 navigation, quickstart, architecture, terminal workflow, evaluation/delivery workflow, and operations pages; source-grounding instructions; local-link and workflow-policy tests; scheduled/manual pinned update workflow limited to review-only `openwiki/` pull requests |
| Behavioral evaluation harness | verified | `daeval` provides provider-neutral trajectory capture, hard correctness checks, soft efficiency expectations, bounded deterministic version-1 reports, category scores, and scripted agent fixtures; contributor guidance and upstream differences are in `docs/EVALUATIONS.md` |
| Harbor sandbox benchmark integration | verified | `daeval/harbor` runs ordered tasks through a required caller-supplied sandbox runner; verifier rewards feed the existing behavioral report, while structural exit-code extraction, failure attribution, deterministic task IDs, Wilson intervals, effect estimates, cancellation, and finite payload/work/time limits have network-free adversarial tests; ContextBench and DRBench records add pinned prompt/output/category metadata without exposing ground truth or passwords |
| Unified cross-model evaluation scorecard | verified | `daeval/scorecard` evaluates one identical bounded task matrix through a required caller-supplied model runner; deterministic per-model Harbor reports, macro category comparisons, aggregate Wilson/MDE statistics, stable leaderboards, cancellation, panic containment, and fair-work rejection have network-free tests |
| Continual-learning-bench system adapter | verified | `daeval/clbench` preserves the pinned one-interaction-per-turn lifecycle, canonical-schema agent reuse, in-state memory threading, deferred observation feedback, stateless reset, structured actions, viewer metadata, artifacts, and usage aggregation behind a required provider-neutral factory; finite turn/schema/prompt/action/file/usage/time limits, cancellation, panic containment, mutation isolation, and network-free adversarial tests |
| Talon long-running agent host | verified | `datalon` provides provider-neutral runtime, channel, and scheduler contracts; ordered lifecycle with rollback and bounded shutdown; per-channel conversation serialization; generation-safe `/stop` cancellation of active and queued turns; concurrent unrelated conversations; a no-model echo fallback; private per-assistant state; and useful finite workspace, recursion, message, send, and shutdown defaults |
| Talon channel tool-approval overrides | verified | `datalon/approval` parses the bounded comma-separated `DEEPAGENTS_TALON_INTERRUPT_ON_TOOLS` overlay, preserves exact case-sensitive names, collapses duplicates, and prepends forced approve/reject rules over same-name base settings for local and MCP tools; dago propagates the combined rules to built-in general-purpose and declarative subagents, while separately compiled/remote subagents remain explicit execution boundaries; the host binds a single pending decision to its exact channel, conversation, and initiating sender, keeps spoofed/stale/ordinary input serialized, and fails closed on absence, timeout, cancellation, malformed prompts, invalid decisions, and scheduled runs |
| Talon Fleet export import | verified | `datalon/fleet` and `cmd/datalon import-fleet` materialize root prompts, skills, and subagents into an explicit assistant state directory; tool manifests generate sanitized OAuth MCP configuration and an operator handoff; transactional managed-path refresh, cancellation, finite archive/file/entry/ratio/tool bounds, zip-slip/link/special-file rejection, and offline adversarial tests cover the pinned behavior |
| Talon MCP tool loading and OAuth login | verified | `datalon/mcp` implements Talon's environment-first user-config resolution, strict bounded `.mcp.json` parsing, HTTP/SSE/stdio sessions, allow/deny filters, duplicate-name rejection, bounded tool results, per-server status, and persistent dynamic-registration OAuth; `datalon mcp config` and paste-back `mcp login <server>` cover the operator workflow with network-free and loopback integration tests |
| Provider-specific MCP OAuth policies | verified | `datalon/mcp/oauthpolicy` selects Slack only for `slack.com` and its subdomains, GitHub device authorization only for `api.githubcopilot.com`, and the MCP authorization-code/PKCE/dynamic-registration policy otherwise; Slack's public-client and optional workspace policy, GitHub's bounded pending/slow-down polling, exact state and loopback callbacks, caller token storage, cancellation, payload/time/poll limits, and fake-transport adversarial tests cover the UI-agnostic contract |
| Talon persistent cron scheduler | verified | `datalon/cron` stores a versioned, atomic, owner-only `cron/jobs.json`; validates one-shot and recurring minute schedules; claims intervals before execution; persists bounded last status/errors; suppresses `[SILENT]` delivery; emits the pinned tick/dispatch/success/failure/delivery lifecycle events; and exposes conversation-scoped create/list/edit/remove tools with finite defaults |
| Talon data lifecycle and retention management | verified | `datalon/lifecycle` preserves the pinned 30-day completed-cron and 24-hour inbound-media defaults with the 1 GiB global artifact ceiling; required state/store dependencies, non-secret dry-run and deletion reports, locked atomic cron replacement, owner-only state, fully bounded walks, cancellation, path-swap rechecks, and fail-closed link/special-file handling have deterministic offline adversarial tests; durable channel sessions and remote traces remain preserved unless explicitly opted in |
| Talon per-run LangSmith tracing | verified | `datalon/tracing` wraps each channel or scheduler runtime invocation in one provider-neutral bounded root span; the LangSmith adapter preserves the pinned project, tags, assistant/conversation/trigger metadata, output/error completion, truthy environment gate, best-effort failure behavior, cancellation, panic propagation, and finite completion deadline with deterministic network-free tests |
| LangSmith tracing management and integration | verified | `datalon/tracing` resolves a required caller credential store against prefixed and canonical environment snapshots, enables stored credentials unless explicitly disabled, isolates the agent project while restoring original subprocess tracing state, disables orphaned tracing, normalizes US/EU and confined loopback endpoints, parses bounded replica projects/endpoints, redacts known credentials from primary/replica runs, and constructs provider sinks through a required factory; cached thread URL resolution is HTTPS-only, bounded, cancellation-aware, UI-agnostic, and covered by network-free adversarial tests |
| Talon Telegram channel | verified | `datalon/telegram` maps private messages, captions, channel posts, sender/message IDs, and bounded media metadata through `datalon.Channel`; exact self/allowlist/open exposure, Bot API authentication, Unicode-safe 4,096-character sends, persistent offsets, bounded long polling/retry/cancellation, and an optional caller-hosted secret-authenticated webhook have network-free adversarial tests |
| Talon WhatsApp channel | verified | `datalon/whatsapp` maps the packaged authenticated loopback Node bridge through `datalon.Channel`; pairing status, self/operator/allowlist/acknowledged-open exposure, snake/camel message aliases, voice media normalization, a per-chunk outbound bot header, 4,096-character sends, private session/media directories, 64 MiB media clamp, and confined local media staging have network-free adversarial Go and Node tests |
| Talon inbound voice transcription | verified | `datalon/speech` recognizes pinned voice/video metadata through a channel wrapper; preserves messages on best-effort failures; uses the pinned Parakeet model and CPU defaults with optional CUDA plus fixed ffmpeg 16 kHz mono conversion and a local Transformers subprocess; supports a caller-authenticated OpenAI-compatible transcription endpoint for non-Parakeet models; accepts legacy speech environment keys; and bounds private media copies, WAVs, subprocess output, transcripts, HTTP bodies, and time |
| General hosted tracing and experiment publication | deferred | optional; live experiment publication is not part of the local deterministic execution contract |
| Asynchronous hosted-subagent lifecycle and durable task state | verified | provider-neutral runner and five management-tool tests |
| Deterministic JavaScript workflows | verified | runtime module metadata export; bounded parallel barriers and item pipelines; structured agent results; phases and logs; cancellation; token, concurrency, lifetime, and collection caps; one-level nesting; background management tools; persisted scripts, journals, agent records, and output; exact-prefix same-session replay; terminal live workflow panel with running-agent elapsed time and streamed token estimates, saved-script launch, refresh, selection, and cancellation with Playwright coverage |
| Remote Agent Protocol background client | verified | thread/run create, status/result, interrupting update, cancellation, auth, path escaping, and redirect-boundary tests |
| Agent Client Protocol v1 server | verified | initialize/auth/new/load/config/prompt/cancel/close lifecycle; factory-backed dynamic model selection with advertised-value validation, history-preserving rebuilds, durable selection restore, and replacement cleanup; replay-marked durable transcripts; image, audio, and embedded content conversion; text, reasoning, tool, progress, and plan projection; approve/reject permission resume; per-session stdio/HTTP/SSE MCP discovery and invocation; stop reasons; and transport tests in `daacp` and `dacode` |
| Auto-mode first-enable notice | verified | versioned install-local acknowledgement; Enter persists and keeps Auto, Esc reverts to Manual and deliberately retries; Go state/render tests and terminal Playwright coverage |
| YOLO acknowledgement gate | verified | policy-versioned, file-locked install-local acknowledgement; YOLO remains inactive until a successful Enter save, `m` selects Manual, Esc keeps the previous mode; persistent status badge plus Go and terminal Playwright coverage |
| LangSmith Studio / Agent Server development API | verified | info and schema discovery; assistant, thread, run, checkpoint state/history/update/fork, store, cancellation, replayable SSE, CORS, generated-wrapper, and config tests |
| Video processing | verified | pluggable extractor contract plus optional bounded FFmpeg adapter; video-window, frame, truncation, fallback, and failure tests |
| Executable upstream conformance provenance | verified | generator validates pinned source paths and test selectors; generated-contract tests strictly decode, validate, mutate, and round-trip every fixture |
| shelley-in-dago end-to-end application | verified | HTTP route tests plus desktop/mobile browser interaction checks |

## Intentional differences

- `damcp` is a UI-neutral trust boundary: project configuration discovery,
  allow-once/remember/deny presentation, OAuth, and reconnect scheduling stay with
  the host. It adds finite file, definition, name, approval, and environment limits
  and requires the user-config path and process-environment lookup positionally.
  Canonical JSON strings and objects match the pinned fingerprints, while numeric
  tokens retain their source spelling; a numeric-only reformat can therefore cause
  a safe extra approval prompt but cannot broaden an existing grant.
- The deprecated Python `model=None` fallback is intentionally absent. A model
  is a mandatory static dependency in Go, and `NewAgent(nil)` panics at the
  construction boundary. Because that deprecated surface was never exported,
  no runtime deprecation adapter is needed; future Go removals use standard
  `Deprecated:` documentation recognized by the toolchain.
- Go exposes one context-aware API rather than Python sync and async variants.
- The pinned client uses a broad TOML manifest and sends `ServerConfig` only to
  its Python development subprocess. Go uses a smaller canonical manifest for
  implemented `dacode` settings and an owner-private versioned JSON file, which
  avoids a new TOML parser boundary. `daconfig.ServerConfig` preserves the typed
  environment contract for caller-owned subprocesses, while the normal Go coding
  agent stays in-process and therefore does not serialize itself. Malformed file
  and server values fail closed; malformed environment values fall through to the
  next lower valid layer, matching the pinned runtime fallback.
- The pinned client automatically probes enabled installed Ollama integrations,
  caches tag results, and also calls `/api/show` for model profiles. Go exposes
  only explicit local `/api/tags` discovery: it has no package-presence concept,
  never sends credentials, does not cache, and leaves refresh timing, profile
  lookup, model construction, and picker integration to the caller. The multi-provider
  resolver can select `ollama:model`, but never invokes discovery automatically.
- Python can import arbitrary provider classes from `class_path`. Go binaries cannot
  safely import packages at runtime, so custom providers are immutable declarations
  paired with caller-supplied compiled factories. The pinned catalog, credential and
  endpoint precedence, model parameters, profile overrides, retries, status, and
  preferences are preserved; missing SDKs are reported as unavailable factories rather
  than guessed as wire-compatible services.
- The pinned client discovers `LANGSMITH_GATEWAY` and
  `LANGSMITH_GATEWAY_API_KEY` from the process environment, then lets installed
  LangChain provider integrations construct models. Go deliberately performs no
  environment or provider-SDK discovery: the application supplies a factory,
  credential, and optional endpoint explicitly. The built-in routes match the
  five gateway-aware providers in the pinned client, while custom exact provider
  maps remain possible. This is gateway routing, not completion of the broader
  multi-provider configuration item.
- The pinned client enables its event socket from process environment and routes
  accepted events directly into its terminal application. Go exposes only the
  reusable `daeventbus` source: there is no implicit environment discovery or
  terminal/runtime wiring, and the required caller sink assigns event authority.
  The JSON-lines vocabulary is preserved, while Go adds finite connection,
  per-connection event, payload, correlation, deadline, and socket-path bounds;
  Windows returns `ErrUnsupported` instead of substituting a network listener.
- The pinned Telegram adapter is long-polling-only and has richer media,
  reaction, status, typing, and edit interfaces than the shared Go
  `datalon.Channel` contract. Go maps bounded media descriptors into message
  metadata without downloading files, fails startup when `getMe` authentication
  fails, and adds an optional caller-hosted webhook handler. Neither delivery
  mode owns a listener, webhook registration, credential discovery, or a vendor
  SDK.
- The WhatsApp Go channel attaches to a caller-supervised packaged Node bridge
  instead of optionally launching that subprocess itself. Its required transport
  is caller-authenticated and SDK-free; the bridge contains the pinned
  `whatsapp-web.js` dependency and owns pairing. Mention allowlists implement the
  pinned case-sensitive `*` and `?` patterns but deliberately omit character-class
  glob syntax. Inbound paths are additionally confined to the configured private
  media directory, while missing confined paths retain the pinned in-progress
  download behavior.
- Talon MCP discovery intentionally limits automatic loading to the two explicit
  environment overrides and the user-level default. It does not silently trust a
  project checkout's MCP configuration. OAuth uses dynamic client registration and
  a pasted loopback callback URL rather than opening a browser or owning a listener.
  The optional provider-policy adapter adds the pinned Slack public authorization-code
  policy and GitHub device flow without adding a vendor SDK or UI. Slack workspace
  selection is an optional interaction capability; GitHub device-code presentation is
  required for that provider. Browser launch, loopback listeners, workspace discovery,
  and terminal presentation remain application responsibilities.
- Talon channel approval adds a finite five-minute default rather than preserving
  the pinned implementation's unbounded wait. Reply routing is also bound to the
  exact channel, conversation, single pending request, and originating sender when
  known; ambiguous, spoofed, stale, and duplicate replies stay on the normal
  serialized message path. Reactions are not part of the shared `datalon.Channel`
  contract, so the portable host supports typed and emoji message replies only.
- Lifecycle durations are typed Go options, with malformed process-environment
  values handled separately by `OptionsFromEnv`. Explicit immediate-cleanup
  booleans preserve upstream zero-day/hour behavior while keeping the all-zero
  options useful and non-destructive. Go also
  adds non-secret dry-run/audit reports, bounded traversal, path replacement
  checks, and optional confined policies for locally persisted channel or trace
  artifacts. Provider session deletion requires an acknowledgement; remote
  LangSmith trace retention remains outside the local filesystem manager.
- Talon tracing creates one explicit root run around each runtime invocation rather
  than installing process-global tracing context. Provider-specific model
  instrumentation may therefore emit separate traces unless the application links
  it explicitly. The caller owns LangSmith client shutdown and flushing.
- General tracing management returns derived agent and restored subprocess
  environments instead of mutating process-global variables. This makes concurrent
  hosts deterministic but requires the application to pass the derived environment
  to subprocesses and use the resolved provider sink explicitly. Thread-link lookup
  is a bounded library service only; browser opening, `/trace`, banner links, and
  other terminal presentation remain outside this slice.
- dago does not download or execute a managed `ripgrep` binary. Every shipped
  backend implements the grep contract directly, avoiding a startup network and
  supply-chain boundary; the `/tools` command exposes the effective tool list.
  Consequently an upstream-style `tools install` command would have no useful
  operation to perform.
- Go's `BaseSandbox` accepts a minimal transport value instead of requiring
  inheritance. Capture-at-source remains opt-in because its POSIX shell and
  coreutils assumptions are not portable to every sandbox image.
- `dabackend/runloop` accepts a narrow caller-authenticated client instead of
  importing the vendor SDK. This keeps credentials, retry policy, and HTTP
  configuration with the application while preserving attached-devbox,
  blueprint, native transfer, timeout, and output behavior. The adapter adds
  finite command, file, batch, error, and pagination bounds that the Python
  partner does not enforce at this layer.
- `dabackend/daytona` likewise accepts a narrow caller-authenticated transport
  instead of bundling the vendor SDK. It validates native batch response paths,
  bounds polling delays and payloads, and attempts session cleanup on a fresh
  bounded context after cancellation; these fail-closed checks are additional
  to the Python partner behavior.
- `dabackend/agentcore` accepts a narrow caller-authenticated transport instead
  of importing the AWS SDK or discovering credentials. It retains the upstream
  region precedence, `deepagents-code` integration source, non-reconnectable
  session lifecycle, execute-result projection, lazy working directory, and
  native file behavior. The Go adapter adds a useful 30-minute command deadline,
  response and lifecycle bounds, and fail-closed resource validation; an explicit
  zero command timeout still relies only on caller cancellation. Provider deletion
  returns a bounded stop error instead of swallowing it so applications can verify
  termination. Registry and command-line selection remain deferred.
- The JavaScript interpreter exposes durable checkpointed-thread state rather
  than Python's additional turn-reset and per-call lifecycle modes. Its PTC
  configuration is a typed tool-name allowlist; JavaScript names are safely
  normalized instead of being rejected solely for identifier syntax.
- Python decorators, runtime imports, reflection-driven runnable composition, and
  class hierarchy are replaced by small Go interfaces.
- dacode renders assistant transcript Markdown through Glamour rather than HTML.
  External strings are terminal-sanitized before CommonMark/GFM parsing and
  styling; Glamour's renderer-generated OSC-8 hyperlinks remain interactive, and
  raw markup never reaches a browser DOM.
- Python can enumerate installed profile plugins through package entry-point
  metadata. Portable Go binaries cannot enumerate imported packages, so profile
  plugins have an explicit-import boundary: applications import the plugin and
  pass its registration callback to `LoadHarnessProfilePlugins` or
  `profile.LoadProviderProfilePlugins`, or the imported package registers from
  `init`. Loaders isolate each callback's error or panic, built-ins load before
  registered overlays, and caller-supplied profile sets retain final precedence.
- Pickle, arbitrary constructor tags, Pydantic and NumPy reconstruction, and
  serialized callables are rejected. This is a security boundary, not missing parity.
- Shell execution is absent unless an explicit sandbox or local-shell backend is
  constructed. The local shell is documented as trusted-host execution, not isolation.
- Provider credentials are explicit adapter inputs or a library-owned OAuth session;
  credentials from another application are never discovered or copied.
- Python's in-process ASGI transport for remote subagents is not applicable in Go;
  async subagents require a caller-supplied runner. The optional Agent Protocol
  package constructs an HTTP-backed runner from URL and authentication options.
- Python package-version inspection is not applied to Go provider adapters. Canonical
  dago messages already preserve tool-call and tool-result identity, so the Nemotron
  pre-serialization compatibility layer is an explicit no-op at the core boundary.
- A tool command targeting its parent is returned as a typed terminal handoff for the
  enclosing Go orchestrator to route. dago does not expose a general-purpose public
  graph builder merely to reproduce Python's graph-composition syntax.
- Video decoding is an opt-in extractor capability. The supplied FFmpeg adapter is
  subprocess-isolated by context and output limits; opaque media behavior remains
  the default and no decoder library is a core dependency.
- General tracing integrations remain separate optional adapters and are not core
  runtime dependencies.
- Public ecosystem constructors keep authority-bearing dependencies positional,
  apply bounded behavior for zero-valued optional limits, and panic on invalid
  static declarations (including negative limits and typed-nil required
  interfaces). External configuration, filesystem, subprocess, transport, and
  runtime failures remain operation errors so callers retain a recovery choice.
- The managed-agent project command is `dago init` to match the Go distribution's
  existing executable. It preserves the pinned starter layout while adding strict
  single-component names, private initial modes, link rejection, and bounded prompt
  input. Deployment and hosted registry operations remain separate CLI slices.
- Managed-agent administration likewise uses `dago agents`. The Go client adds
  finite page, response, request, retry, cursor, and error bounds, refuses redirects,
  and disables ambient proxy use in the built-in CLI transport. Create/update and
  directory synchronization use the separate `dago deploy` slice.
- The managed deployment client sends tool references as declared and leaves their
  workspace registration validation to the managed service. It does not perform the
  Python command's extra preflight MCP name/URL-to-ID resolution or cache that
  mapping locally. Project-local deployment state and environment files are ignored;
  layered environment loading is a separate configuration boundary.
- Managed MCP registration accepts HTTPS URLs only and rejects credentials and
  fragments, a stricter boundary than the upstream generic URL client. This slice
  registers OAuth-mode records and uses the managed service's generic per-user
  provider/session workflow. Service-specific enterprise consent or policy adapters
  remain part of the separate provider-policy slice.
- The local Agent Server implements the Studio and SDK surface needed for dago
  development. It does not execute arbitrary Python/JavaScript graph imports, host
  custom web applications, schedule crons, provide LangSmith tracing, or implement
  the protocol-v2 long-lived thread websocket. Go graph paths are compiled into a
  generated wrapper, and runs use the existing dago graph rather than a general
  LangGraph runtime.
- The ACP adapter implements the stable version 1 baseline, session close, dynamic
  model Session Config Options for factory-backed sessions, and durable session load
  when the configured factory returns a `SessionLoader`. Model changes rebuild only the
  addressed session and validate the requested identifier against the advertised list.
  Durable model selection additionally requires `SessionConfigSaver`; factories must
  share the session checkpointer so the replacement runner continues existing history.
  Loading verifies
  the original persisted working directory before replaying human, assistant, tool-call,
  and tool-result updates with replay metadata. Configuration options and session-scoped
  HTTP/SSE MCP servers are advertised when configured. Session list/resume, modes,
  additional workspace roots, ACP-routed MCP transport, and client-owned filesystem and
  terminal operations remain outside the adapter. Human approval maps only approve and
  reject decisions because ACP permission choices do not carry edited tool arguments or
  free-form responses.
- The pinned client always scaffolds and starts a Python development server and
  routes its coding-agent calls over HTTP/SSE. The Go terminal keeps its native
  runner in-process by default and exposes the process boundary only when the
  operator supplies `--local-dev-server` with an absolute executable and literal
  argv. It does not download a server, discover an interpreter, reserve a public
  port, inherit credentials, or pretend an arbitrary companion implements an
  agent protocol. The resulting supervisor and restart seam preserve the pinned
  lifecycle guarantees without adding implicit process or network authority.
- Core constructors distinguish static assembly from fallible loading. In-memory,
  state, composite, store, base-sandbox, Context Hub, and existing-remote-sandbox
  constructors return ready values and panic on invalid static dependencies. Typed
  tool declarations and internal delta-channel specifications follow the same rule;
  model input validation, tool execution, and checkpoint restoration remain
  recoverable operation errors.
  `dabackend.LoadMemory` and `dabackend.LoadState` retain recoverable errors for
  untrusted snapshots, while filesystem, local-shell, Docker, checkpoint-open, and
  server constructors retain errors for I/O, entropy, remote, or persisted-state
  failures.
- Mandatory inputs are positional: composite defaults, store namespace factories,
  Docker images, Agent Server graph registrations, ACP session factories, and
  Shelley's loop model and SQLite data source no longer hide in configuration
  structs. Browser application
  construction is network-free and returns a ready value; browser-store restoration
  remains the fallible I/O boundary. Zero-valued bounds, including Shelley's SQLite
  reader count, select finite production defaults; negative static bounds are rejected
  rather than silently reinterpreted as defaults.
